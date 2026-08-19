package server

// docs/plans/ingestion-identity.md PR-4 (B-5): 「readonly の trigger job から
// task_create が通る」を実際に通して pin する。 doc は「この PR の前提その
// もの」と明記しているので、推論(「boidPolicy は Role/Readonly を見ないので
// 通るはずだ」)だけで済ませず、本番の実行パスを実際に走らせる。
//
// internal/api.TaskWorkflowService.fireTrigger (trigger_loop.go) が dispatch
// する exec job は api.StartExecRequest{Readonly: true, Argv: ["sh","-c",
// trig.Run]} — sandbox 内で trig.Run (例: `boid task create ...`) が動くと、
// そのプロセスは boid_shim 経由でこの daemon プロセスの broker に BoidOp を
// 送る。broker がそれを許可した後に実際にオペレーションを実行するのが
// boidBuiltinExecutor.ExecuteBoidBuiltin (boid_executor.go) — このテストは
// その実行 (task_create) が本当に成功することを確認する。
//
// 「readonly は関係ない」という主張の根拠は構造的に確認できる:
//   - executor が受け取る唯一のコンテキストは sandbox.TokenContext
//     (protocol.go) で、フィールドは JobID/TaskID/ProjectID/WorkspaceID/
//     AllowedProjectIDs/Role/ProjectDir/SandboxRoot の 8 つだけ — Readonly
//     に相当するフィールドが存在しない。job が readonly かどうかという情報
//     自体が、この層まで一切運ばれていない
//   - internal/orchestrator/policy.go の boidPolicy(_ Role, pctx
//     PolicyContext) も同様: pctx (PolicyContext) は ProjectDir/HomeDir の
//     2 フィールドのみで Readonly を持たず、第一引数の Role すら `_` で
//     無視される。「boid」op の allowlist は常に同じ 1 通りしかない
//
// つまり readonly は internal/dispatcher/session_job.go の
// Visibility.Writable (サンドボックスのファイルシステム mount mode) だけを
// 制御し、BoidOp の allowlist にも executor にも一切伝播しない。このテストは
// 「readonly だから通らないのでは」という懸念そのものが起こりようがないこと
// を、実際に task_create を通すことで確認する。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBoidBuiltinExecutor_TaskCreate_SucceedsFromWhatATriggerJobsSandboxWouldSend(t *testing.T) {
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{"triage": {}},
	}}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: store, Meta: meta},
	}

	// The ONLY context ExecuteBoidBuiltin ever receives — see this file's
	// doc comment for why sandbox.TokenContext having no Readonly field is
	// exactly the point. This is otherwise an ordinary in-workspace
	// task_create request, the same shape a trigger's `run` script would
	// issue via `boid task create` from inside a Readonly:true exec job.
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"jira:ROOKPF-1: something","behavior":"triage"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task_create from a trigger-shaped call: exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1 — task_create must actually succeed, not merely be policy-allowed", len(store.created))
	}
}

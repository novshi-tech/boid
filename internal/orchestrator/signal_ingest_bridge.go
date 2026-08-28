package orchestrator

// docs/plans/boid-internal-signal-inbox.md (PR-1): boid 自身の action 列を
// signal inbox (signals テーブル) へ統合する側の実装。CreateAction (store.go)
// が action の INSERT と同一 tx でここを呼ぶ。判定の詳細は doc §4.2〜§4.6、
// 層分離の理由は §6.2 を参照 — PR description にも同じ理由を書く。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// InternalSignalPack / InternalSignalConnector are the reserved "<pack>/
// <connector>" identity boid's own action-derived signals ingest under
// (§4.6's envelope table: source.pack="boid", source.connector="actions").
// InternalSignalPack is a reserved Pack name — internal/integrationpack's
// LoadPacks refuses to ever load a real installed Pack claiming it, so an
// external connector's rows can never collide with these under the same
// signals PRIMARY KEY (workspace_id, service, connector, id).
const (
	InternalSignalPack      = "boid"
	InternalSignalConnector = "actions"
)

// MetaProjectResolver answers, for a workspace, which of its projects
// declare signals.sources[] in their (hydrated) project.yaml — its
// metaprojects (docs/plans/boid-internal-signal-inbox.md §4.3/§6.2:
// "メタプロジェクトとは signals.sources[] を宣言している project のこと").
// Backed in production by *ProjectStore.MetaProjectIDs — the SAME hydrated
// meta cache every other project.yaml-derived read already uses, injected
// here as a narrow interface (mirroring internal/server/boid_executor.go's
// resolveOrCaptureService/actionListService/projectLookup narrowing
// convention) rather than depending on *ProjectStore's full surface. See
// this package's TaskRepository.SetMetaProjectResolver for the wiring seam
// and the design doc's §6.2 for why the lookup lives here (an
// internal/orchestrator-owned interface, satisfied by an
// internal/orchestrator-owned type) rather than being pushed up to
// internal/api/internal/server the way internal/integrationpack's Pack
// registry had to be (§6.2's own comparison): *ProjectStore already lives in
// this package, so — unlike the Pack registry, which lives in a sibling
// package internal/orchestrator does not and must not import — there is no
// layering violation to route around, only an instance-wiring one (a bare
// package-level CreateAction has no constructor-injected fields of its own),
// which TaskRepository's existing late-binding-setter shape (SetWorkspaceStore/
// SetHostCommands on *ProjectStore itself) already solves for the identical
// problem.
type MetaProjectResolver interface {
	// MetaProjectIDs returns the project ids within workspaceID that declare
	// signals.sources[] — nil/empty when none do, including when the
	// resolver has no data at all for workspaceID. §6.2 treats both the same
	// way: no metaproject to ingest FOR, so nothing to ingest.
	MetaProjectIDs(workspaceID string) []string
}

// var _ MetaProjectResolver = (*ProjectStore)(nil) proves *ProjectStore
// (project_store.go's MetaProjectIDs) satisfies this interface against the
// real production type, the same "compile-time proof, not just a hopeful
// runtime type assertion" pattern signal_store.go's own package doc comment
// points at for api.SignalStore/*orchestrator.TaskRepository.
var _ MetaProjectResolver = (*ProjectStore)(nil)

type writerProjectContextKey struct{}

// WithWriterProjectID marks ctx as carrying a sandbox write's origin project
// (docs/plans/boid-internal-signal-inbox.md §4.3/§6.2) — the same
// context-propagation idiom actor.go's WithActor/ActorFromContext already
// established for Action.Actor, applied to the one additional fact
// CreateAction's ingest decision needs that Actor cannot supply:
// internal/api/task_service.go's UpdateTask stamps ActorHuman even for a
// sandbox-originated call (its own "Actor caveat" comment says so), so actor
// is not trustworthy evidence of the writer's project.
//
// internal/server/boid_executor.go's ExecuteBoidBuiltin is the ONLY call
// site that should ever call this (§6.2: "運搬は1箇所で閉じられる") — every
// sandbox-originated write funnels through it with a sandbox.TokenContext
// carrying ProjectID, and every OTHER ctx-building path in this codebase
// (HTTP handlers via WithActor(r.Context(), ActorHuman), daemon loops via
// WithActor(ctx, ActorDaemon)) never touches this key. WriterProjectIDFromContext
// therefore reports "not sandbox-originated" for every human/daemon write
// with zero special-casing anywhere else in the call graph.
//
// projectID may be "" — a sandbox write whose TokenContext.ProjectID could
// not be resolved still calls this (with an empty string), which is exactly
// what lets WriterProjectIDFromContext distinguish "no sandbox writer at
// all" (ingest-eligible on this axis) from "sandbox writer, but its project
// is unknown" (fail-close, §4.3).
func WithWriterProjectID(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, writerProjectContextKey{}, projectID)
}

// WriterProjectIDFromContext returns the project id WithWriterProjectID
// stamped, and whether ctx carries one at all. ok==false means this write
// was never routed through ExecuteBoidBuiltin (a human/daemon/HTTP-originated
// write) — CreateAction's actor-axis check treats that as "not a sandbox
// write, self-reference cannot apply". ok==true with an empty projectID
// means a sandbox write whose project could not be resolved — the fail-close
// case (§4.3).
func WriterProjectIDFromContext(ctx context.Context) (projectID string, ok bool) {
	v := ctx.Value(writerProjectContextKey{})
	if v == nil {
		return "", false
	}
	projectID, ok = v.(string)
	return projectID, ok
}

// IngestActionSignal is CreateAction's internal-signal ingest step (design
// doc §4). Exported — rather than a private helper only CreateAction calls —
// so tests can exercise the ingest DECISION in isolation from the actions
// table's own id PRIMARY KEY (a real action id can only ever be INSERTed
// once, so a test proving re-ingest of the SAME action id is a no-op, Q10,
// has to call this directly with a repeated Action value rather than
// through CreateAction twice).
//
// Best-effort by design (§6.2: "ingest 失敗で action の書き込みを巻き戻さな
// い"). CreateAction calls this AFTER its own INSERT has already run (as one
// statement within the caller's transaction) and only logs whatever error
// this returns: a failed ingest means the workspace misses ONE signal for
// this action, which is recoverable — the same card's next action carries
// its own signal (§6.2's "取りこぼした signal は次に同じ card が動いたとき
// の signal でカバーされる"). It must never fail the caller's transaction:
// the action is the fact, the signal is a side effect of it, and undoing the
// fact because the side effect failed would invert that causality (§6.2).
func IngestActionSignal(ctx context.Context, dbtx db.DBTX, a *Action, resolver MetaProjectResolver) error {
	if resolver == nil || a == nil || a.TaskID == "" {
		return nil
	}

	taskType, projectID, err := actionTargetTypeAndProject(dbtx, a.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("internal signal ingest: resolve target task: %w", err)
	}
	// §4.2 対象軸 — ingest の範囲は card 型 task 宛の action だけ。
	// api_gateway_request のような exec task 宛の大量の行はここで構造的に
	// 落ちる (Q9)。
	if taskType != TaskTypeCard {
		return nil
	}

	workspaceID, err := projectWorkspaceID(dbtx, projectID)
	if err != nil {
		return fmt.Errorf("internal signal ingest: resolve target project workspace: %w", err)
	}
	if workspaceID == "" {
		// project がどの workspace にも紐付いていない — ingest 先が無い。
		return nil
	}

	metaProjectIDs := resolver.MetaProjectIDs(workspaceID)
	if len(metaProjectIDs) == 0 {
		// §6.2: signals を宣言した project が workspace に一つも無ければ
		// ingest しない — 判定するまでもなく何も起きない (Q5)。
		return nil
	}

	// §4.3 actor 軸 — 書き込み元 project による自己参照の遮断、および
	// fail-close。actor 文字列 (task:<id> 等) は判定材料にしない — Q8。
	if writerProjectID, hasWriter := WriterProjectIDFromContext(ctx); hasWriter {
		if writerProjectID == "" {
			// fail-close: TokenContext は持つが書き込み元 project を解決
			// できなかった (Q7)。
			return nil
		}
		for _, mp := range metaProjectIDs {
			if mp == writerProjectID {
				// 自己参照 — メタプロジェクト自身の job/task が書いた
				// (Q8)。複数メタプロジェクトが居ても全部が対象になる
				// (Q12)。
				return nil
			}
		}
	}

	row := SignalIngestRow{
		ID:         a.ID,
		OccurredAt: a.CreatedAt.UTC().Format(time.RFC3339Nano),
		Identity:   a.TaskID,
		Author:     a.Actor,
		Title:      a.Type,
	}
	return IngestSignals(dbtx, workspaceID, "", InternalSignalPack+"/"+InternalSignalConnector, []SignalIngestRow{row})
}

// actionTargetTypeAndProject is a minimal, single-row lookup (type + owning
// project id only) rather than the full GetTask (which additionally runs
// child-count rollup subqueries) — CreateAction's ingest step runs on every
// single action write, so it deliberately avoids that extra cost.
func actionTargetTypeAndProject(dbtx db.DBTX, taskID string) (taskType TaskType, projectID string, err error) {
	var t string
	err = dbtx.QueryRow(`SELECT type, project_id FROM tasks WHERE id = ?`, taskID).Scan(&t, &projectID)
	return TaskType(t), projectID, err
}

// projectWorkspaceID resolves projectID's workspace via project_workspaces
// (the same table action_list.go's WorkspaceID scoping joins against) —
// "" (with a nil error) when the project is not linked to any workspace,
// matching how the rest of this ingest step treats "nothing to ingest into"
// as a quiet no-op rather than an error.
func projectWorkspaceID(dbtx db.DBTX, projectID string) (string, error) {
	var workspaceID string
	err := dbtx.QueryRow(`SELECT workspace_id FROM project_workspaces WHERE project_id = ?`, projectID).Scan(&workspaceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return workspaceID, nil
}

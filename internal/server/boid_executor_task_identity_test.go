package server

// docs/plans/ingestion-identity.md PR-1 (B-1): boid_executor.go's
// task_identity_link / _unlink / _resolve cases. Broker-side scoping
// (project_id) is covered separately (internal/sandbox's
// TestBroker_BoidTaskIdentity*_ProjectIDDenied) — these pin the
// executor-side task_id ownership check (Link only, mirroring
// BoidOpActionSend/BoidOpTaskWake's own GetTask+AllowsProject pattern), the
// store-error -> ExecResponse translation (including
// BoidOpTaskIdentityResolve's distinguished "not found" exit code and
// BoidOpTaskIdentityLink's distinguished "conflict" exit code), and the
// project-match / prefix-id fixes from the PR #968 Opus review.
//
// A few of these (the ones needing a REAL sqlite-backed store to catch a
// genuine FOREIGN KEY constraint or a real GetTask prefix fallback —
// capturingTaskStore below does neither) build their own :memory: DB
// directly via internal/db + internal/db/migrate rather than
// testutil.NewTestDB: this file is `package server` (white-box), and
// testutil imports internal/server (for testutil.NewTestServer), so
// importing testutil here would be an import cycle.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// newBoidExecutorTestDB opens a fresh :memory: sqlite DB with migrations
// applied — see the file-level comment for why this doesn't just use
// testutil.NewTestDB.
func newBoidExecutorTestDB(t *testing.T) db.DBTX {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d.Conn
}

// fakeIdentityStore is a minimal api.TaskIdentityStore double.
type fakeIdentityStore struct {
	linkCalls   []fakeLinkCall
	unlinkCalls []fakeUnlinkCall

	linkErr    error
	unlinkErr  error
	resolveErr error
	resolved   *orchestrator.Task
}

type fakeLinkCall struct {
	projectID, identity, taskID string
}
type fakeUnlinkCall struct {
	projectID, identity string
}

func (f *fakeIdentityStore) LinkIdentity(projectID, identity, taskID string) error {
	f.linkCalls = append(f.linkCalls, fakeLinkCall{projectID, identity, taskID})
	return f.linkErr
}
func (f *fakeIdentityStore) UnlinkIdentity(projectID, identity string) error {
	f.unlinkCalls = append(f.unlinkCalls, fakeUnlinkCall{projectID, identity})
	return f.unlinkErr
}
func (f *fakeIdentityStore) UnlinkAllForTask(taskID string) error { return nil }
func (f *fakeIdentityStore) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.resolved, nil
}
func (f *fakeIdentityStore) ListIdentitiesByTask(taskID string) ([]string, error) { return nil, nil }

func TestBoidBuiltinExecutor_TaskIdentityLink_CallsStore(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusCaptured},
	}}
	identities := &fakeIdentityStore{}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store, Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityLink,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
		TaskID:    "t1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(identities.linkCalls) != 1 {
		t.Fatalf("LinkIdentity call count = %d, want 1", len(identities.linkCalls))
	}
	got := identities.linkCalls[0]
	if got.projectID != "proj-1" || got.identity != "jira:X-1" || got.taskID != "t1" {
		t.Fatalf("LinkIdentity call = %+v, want {proj-1 jira:X-1 t1}", got)
	}
}

// TestBoidBuiltinExecutor_TaskIdentityLink_RejectsTaskOutsideWorkspace pins
// the executor-side defense-in-depth check on TaskID's OWN project — the
// broker only validates the ProjectID field itself, not what project the
// caller-supplied TaskID actually belongs to (it has no TaskStore to look
// that up with). Mirrors BoidOpActionSend/BoidOpTaskWake exactly.
func TestBoidBuiltinExecutor_TaskIdentityLink_RejectsTaskOutsideWorkspace(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-outside", Status: orchestrator.TaskStatusCaptured},
	}}
	identities := &fakeIdentityStore{}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store, Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityLink,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
		TaskID:    "t1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Fatalf("stderr = %q, want mention of workspace scoping", resp.Stderr)
	}
	if len(identities.linkCalls) != 0 {
		t.Fatalf("LinkIdentity must not be called when the task is outside the workspace, got %d calls", len(identities.linkCalls))
	}
}

// TestBoidBuiltinExecutor_TaskIdentityLink_ConflictSurfacesAsDistinctExitCode
// pins docs/plans/ingestion-identity.md's PR-2 section: a workspace doing
// resolve_or_capture needs to tell "this identity already points elsewhere"
// apart from any other failure MECHANICALLY, via ExecResponse.ExitCode —
// not by pattern-matching Stderr's exact wording, which is not a public
// contract (Opus review, PR #968). Stderr is still populated for a human
// reading `boid task identity link` output directly, but this test
// deliberately does NOT assert its exact text.
func TestBoidBuiltinExecutor_TaskIdentityLink_ConflictSurfacesAsDistinctExitCode(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusCaptured},
	}}
	identities := &fakeIdentityStore{linkErr: orchestrator.ErrIdentityConflict}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store, Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityLink,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
		TaskID:    "t1",
	})
	if resp.ExitCode != sandbox.IdentityConflictExitCode {
		t.Fatalf("exit code = %d, want IdentityConflictExitCode (%d) — a conflict must be distinguishable from a generic failure (exit 1) or a resolve miss (IdentityNotFoundExitCode) purely by exit code", resp.ExitCode, sandbox.IdentityConflictExitCode)
	}
}

// TestBoidBuiltinExecutor_TaskIdentityLink_ResolvesPrefixTaskID pins the
// Blocker from the Opus review: e.tasks.GetTask supports an >=8-char prefix
// fallback (internal/orchestrator/store.go), so a caller may legitimately
// pass a short task id — but LinkIdentity writes whatever id it's given
// straight into task_identities.task_id, an FK column. Before the fix, the
// executor passed the CALLER'S raw (possibly-prefix) req.TaskID to
// LinkIdentity instead of the GetTask call's resolved existing.ID, which
// blows up as a raw SQLite "FOREIGN KEY constraint failed" once the FK is
// enforced for real (capturingTaskStore doesn't enforce FKs, so this needs a
// real sqlite-backed store to catch — see also the exact ID passed to
// LinkIdentity below).
func TestBoidBuiltinExecutor_TaskIdentityLink_ResolvesPrefixTaskID(t *testing.T) {
	d := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(d, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if len(task.ID) < 8 {
		t.Fatalf("generated task id %q is too short to exercise the >=8-char prefix fallback", task.ID)
	}
	prefix := task.ID[:8]

	store := apiTxStore{tasks: orchestrator.NewTaskRepository(d)}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store, Identities: store}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityLink,
		ProjectID: "proj-1",
		Identity:  "jira:PREFIX-1",
		TaskID:    prefix,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q) — a prefix task id must resolve, not hit a raw FK constraint error", resp.ExitCode, resp.Stderr)
	}

	resolved, err := orchestrator.ResolveIdentity(d, "proj-1", "jira:PREFIX-1")
	if err != nil {
		t.Fatalf("resolve after link: %v", err)
	}
	if resolved.ID != task.ID {
		t.Fatalf("resolved task id = %q, want the FULL id %q (not the prefix %q the caller passed)", resolved.ID, task.ID, prefix)
	}
}

// TestBoidBuiltinExecutor_TaskIdentityLink_RejectsCrossProjectTaskWithinWorkspace
// pins the second Opus review finding: AllowsProject alone only checks the
// task's project is SOMEWHERE in the caller's workspace, not that it matches
// the SPECIFIC req.ProjectID the identity is scoped under (I-3). Before the
// fix, linking proj-1's identity to a task that actually lives in proj-2
// (both in the same workspace) succeeded silently.
func TestBoidBuiltinExecutor_TaskIdentityLink_RejectsCrossProjectTaskWithinWorkspace(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-2", Status: orchestrator.TaskStatusCaptured},
	}}
	identities := &fakeIdentityStore{}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store, Identities: identities}}
	// Both proj-1 and proj-2 are in the SAME workspace (both allowed) — the
	// existing AllowsProject check alone must not be enough to let this
	// through.
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1", "proj-2"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityLink,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
		TaskID:    "t1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (task's actual project proj-2 does not match req.ProjectID proj-1)", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "project") {
		t.Fatalf("stderr = %q, want mention of the project mismatch", resp.Stderr)
	}
	if len(identities.linkCalls) != 0 {
		t.Fatalf("LinkIdentity must not be called when the task's project does not match req.ProjectID, got %d calls", len(identities.linkCalls))
	}
}

// TestBoidBuiltinExecutor_TaskIdentityResolve_RejectsTaskOutsideWorkspace
// pins the third Opus review finding: every OTHER op that hands a task back
// to the caller (BoidOpTaskGet / BoidOpTaskTriageGet / BoidOpActionSend /
// BoidOpTaskWake) re-checks AllowsProject on the task it actually got, not
// just the project the caller asked about — resolve was the one op that
// didn't.
func TestBoidBuiltinExecutor_TaskIdentityResolve_RejectsTaskOutsideWorkspace(t *testing.T) {
	identities := &fakeIdentityStore{resolved: &orchestrator.Task{ID: "t1", ProjectID: "proj-outside", Status: orchestrator.TaskStatusTriaged}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityResolve,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (resolved task's project proj-outside is outside the workspace)", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Fatalf("stderr = %q, want mention of workspace scoping", resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "t1") {
		t.Fatalf("stdout = %q, must not leak the out-of-workspace task id", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskIdentityUnlink_CallsStore(t *testing.T) {
	identities := &fakeIdentityStore{}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityUnlink,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(identities.unlinkCalls) != 1 || identities.unlinkCalls[0] != (fakeUnlinkCall{"proj-1", "jira:X-1"}) {
		t.Fatalf("UnlinkIdentity calls = %+v, want one call {proj-1 jira:X-1}", identities.unlinkCalls)
	}
}

func TestBoidBuiltinExecutor_TaskIdentityResolve_Found(t *testing.T) {
	identities := &fakeIdentityStore{resolved: &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusTriaged}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityResolve,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "t1") || !strings.Contains(resp.Stdout, "triaged") {
		t.Fatalf("stdout = %q, want it to mention task id and status", resp.Stdout)
	}
}

// TestBoidBuiltinExecutor_TaskIdentityResolve_NotFound pins the design doc's
// explicit contract: "未登録は「見つからない」を exit code で表し、エラーに
// しない" — a distinguished exit code, not the generic ExitCode:1 error path.
func TestBoidBuiltinExecutor_TaskIdentityResolve_NotFound(t *testing.T) {
	identities := &fakeIdentityStore{resolveErr: orchestrator.ErrTaskNotFound}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityResolve,
		ProjectID: "proj-1",
		Identity:  "jira:UNKNOWN",
	})
	if resp.ExitCode != sandbox.IdentityNotFoundExitCode {
		t.Fatalf("exit code = %d, want IdentityNotFoundExitCode (%d)", resp.ExitCode, sandbox.IdentityNotFoundExitCode)
	}
	if resp.Stderr != "" {
		t.Fatalf("stderr = %q, want empty (not found is not an error)", resp.Stderr)
	}
}

// TestBoidBuiltinExecutor_TaskIdentityResolve_OtherErrorStaysGeneric confirms
// a NON-not-found store error still uses the ordinary ExitCode:1 error path
// — only ErrTaskNotFound gets the special code.
func TestBoidBuiltinExecutor_TaskIdentityResolve_OtherErrorStaysGeneric(t *testing.T) {
	identities := &fakeIdentityStore{resolveErr: errors.New("db exploded")}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Identities: identities}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityResolve,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 for a non-not-found error", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "db exploded") {
		t.Fatalf("stderr = %q, want it to surface the underlying error", resp.Stderr)
	}
}

// TestBoidBuiltinExecutor_TaskIdentity_Unavailable pins the "Identities not
// configured" guard for Unlink/Resolve (no TaskID lookup, so a bare
// TaskAppService{} is a realistic stand-in). Link is covered separately
// below (TestBoidBuiltinExecutor_TaskIdentityLink_Unavailable) since it
// needs a working TaskStore for the task-ownership check BEFORE it ever
// reaches the Identities guard — a bare TaskAppService{} would fail on that
// unrelated nil TaskStore first, not exercise the guard this test wants.
func TestBoidBuiltinExecutor_TaskIdentity_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	for _, req := range []*sandbox.BoidRequest{
		{Op: sandbox.BoidOpTaskIdentityUnlink, ProjectID: "proj-1", Identity: "jira:X-1"},
		{Op: sandbox.BoidOpTaskIdentityResolve, ProjectID: "proj-1", Identity: "jira:X-1"},
	} {
		resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, req)
		if resp.ExitCode != 1 {
			t.Fatalf("op %s: exit code = %d, want 1 when Identities is unset", req.Op, resp.ExitCode)
		}
	}
}

func TestBoidBuiltinExecutor_TaskIdentityLink_Unavailable(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusCaptured},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store}}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskIdentityLink,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
		TaskID:    "t1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 when Identities is unset", resp.ExitCode)
	}
}

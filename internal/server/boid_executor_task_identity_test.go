package server

// docs/plans/ingestion-identity.md PR-1 (B-1): boid_executor.go's
// task_identity_link / _unlink / _resolve cases. Broker-side scoping
// (project_id) is covered separately (internal/sandbox's
// TestBroker_BoidTaskIdentity*_ProjectIDDenied) — these pin the
// executor-side task_id ownership check (Link only, mirroring
// BoidOpActionSend/BoidOpTaskWake's own GetTask+AllowsProject pattern) and
// the store-error -> ExecResponse translation, including
// BoidOpTaskIdentityResolve's distinguished "not found" exit code.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

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

func TestBoidBuiltinExecutor_TaskIdentityLink_ConflictSurfacesAsGenericError(t *testing.T) {
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
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (conflict is a generic error, not the not-found exit code)", resp.ExitCode)
	}
	if resp.Stderr != orchestrator.ErrIdentityConflict.Error() {
		t.Fatalf("stderr = %q, want the ErrIdentityConflict message verbatim (%q)", resp.Stderr, orchestrator.ErrIdentityConflict.Error())
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

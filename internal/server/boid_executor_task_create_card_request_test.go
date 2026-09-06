package server

// Pins BoidOpTaskCreate's card-command launcher ownership check: a
// launcher job's `boid task create` claims its own card_requests row's
// slot in the same transaction as the task insert, but only when the
// calling job actually owns that request (LauncherJobID == ctx.JobID,
// CardID matches, status is launching). Runs against a real sqlite DB,
// same precedent as boid_executor_task_create_idempotency_key_test.go.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBoidOpTaskCreate_OwnedCardRequest_AttachesAtomically(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(conn)
	card := &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	cardReq := &orchestrator.CardRequest{
		CardID:        card.ID,
		Status:        orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-launcher",
	}
	if err := orchestrator.CreateCardRequest(conn, cardReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}

	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{
			Tasks:             repo,
			CardRequestLinker: repo,
			Meta:              executorMetaStub{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"executor": {}}}},
		},
		cardRequests: repo,
	}
	ctx := sandbox.TokenContext{
		ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"},
		JobID: "job-launcher", CardID: card.ID, CardRequestID: cardReq.ID,
	}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"review findings","initial_status":"pending","behavior":"executor"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	got, err := repo.GetCardRequest(cardReq.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached {
		t.Errorf("status = %q, want attached", got.Status)
	}
	if got.TargetKind != orchestrator.CardRequestTargetKindTask {
		t.Errorf("target_kind = %q, want task", got.TargetKind)
	}
	if got.TargetID == "" {
		t.Fatal("target_id is empty, want the created task's id")
	}

	task, err := repo.GetTask(got.TargetID)
	if err != nil {
		t.Fatalf("GetTask(%q): %v", got.TargetID, err)
	}
	if task.ParentID != "" {
		t.Errorf("ParentID = %q, want empty — a card-command launcher's continuation is a root task", task.ParentID)
	}
	// Idempotency default: since the script omitted `ref:`, the daemon
	// defaults it to the request id, so a retried `boid task create` from a
	// re-run launcher converges onto the same task instead of planting a
	// second one.
	if task.Ref != cardReq.ID {
		t.Errorf("Ref = %q, want the request id %q defaulted in", task.Ref, cardReq.ID)
	}
}

// TestBoidOpTaskCreate_StaleLauncher_NotAttached pins that a job whose
// token names a card_requests id it does NOT currently own (a superseded
// launcher, or simply the wrong job) creates the task normally but does
// NOT attach it to that request — the request is left exactly as it was,
// for the recovery/retry path to handle, rather than silently hijacking
// someone else's slot.
func TestBoidOpTaskCreate_StaleLauncher_NotAttached(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(conn)
	card := &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	cardReq := &orchestrator.CardRequest{
		CardID:        card.ID,
		Status:        orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-current",
	}
	if err := orchestrator.CreateCardRequest(conn, cardReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}

	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{
			Tasks:             repo,
			CardRequestLinker: repo,
			Meta:              executorMetaStub{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"executor": {}}}},
		},
		cardRequests: repo,
	}
	// A DIFFERENT job id names the same card_request — a stale/superseded
	// launcher, or an attacker-controlled sandbox trying to hijack the slot.
	ctx := sandbox.TokenContext{
		ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"},
		JobID: "job-stale", CardID: card.ID, CardRequestID: cardReq.ID,
	}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"should not attach","initial_status":"pending","behavior":"executor"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	got, err := repo.GetCardRequest(cardReq.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("status = %q, want unchanged launching (stale caller must not attach)", got.Status)
	}
	if got.TargetID != "" {
		t.Errorf("target_id = %q, want empty (stale caller must not attach)", got.TargetID)
	}
}

// TestBoidOpTaskCreate_ChildCreate_NotTreatedAsRequestContinuation pins that
// a create carrying an explicit parent_id must never be treated as the
// launcher's own continuation, even if the caller's token happens to carry
// card context.
func TestBoidOpTaskCreate_ChildCreate_NotTreatedAsRequestContinuation(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(conn)
	card := &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	parent := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTask(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	cardReq := &orchestrator.CardRequest{
		CardID:        card.ID,
		Status:        orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-launcher",
	}
	if err := orchestrator.CreateCardRequest(conn, cardReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}

	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{
			Tasks:             repo,
			CardRequestLinker: repo,
			Meta:              executorMetaStub{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"executor": {}}}},
		},
		cardRequests: repo,
	}
	ctx := sandbox.TokenContext{
		ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"},
		JobID: "job-launcher", CardID: card.ID, CardRequestID: cardReq.ID,
	}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"a normal child","parent_id":"` + parent.ID + `","ref":"child-1","behavior":"executor"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	got, err := repo.GetCardRequest(cardReq.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("status = %q, want unchanged launching — a non-root create must not consume the request's slot", got.Status)
	}
}

// TestBoidOpTaskCreate_LauncherSuppliedRef_StillAttaches pins the P1 fix: a
// launcher-supplied `ref` that CreateTask's own get-or-create resolves to
// an already-existing task (e.g. a retried launcher run, or one that
// reused a ref from an earlier unrelated create) must still attach that
// task to the request — otherwise the request stays "launching" forever
// and the card's slot is stuck.
func TestBoidOpTaskCreate_LauncherSuppliedRef_StillAttaches(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(conn)
	card := &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	// A pre-existing task already owns "issue-123" as a root ref — simulates
	// a launcher reusing a ref it (or something else) created earlier.
	preexisting := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: "issue-123", Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTask(preexisting); err != nil {
		t.Fatalf("create preexisting: %v", err)
	}
	cardReq := &orchestrator.CardRequest{
		CardID:        card.ID,
		Status:        orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-launcher",
	}
	if err := orchestrator.CreateCardRequest(conn, cardReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}

	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{
			Tasks:             repo,
			CardRequestLinker: repo,
			Meta:              executorMetaStub{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"executor": {}}}},
		},
		cardRequests: repo,
	}
	ctx := sandbox.TokenContext{
		ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"},
		JobID: "job-launcher", CardID: card.ID, CardRequestID: cardReq.ID,
	}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"reused ref","ref":"issue-123","behavior":"executor"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	got, err := repo.GetCardRequest(cardReq.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached {
		t.Fatalf("status = %q, want attached — a ref hit must not leave the slot stuck launching", got.Status)
	}
	if got.TargetID != preexisting.ID {
		t.Errorf("target_id = %q, want the pre-existing task %q (ref get-or-create)", got.TargetID, preexisting.ID)
	}
}

// TestBoidOpTaskCreate_CardTypeCreate_NeverAttaches pins the P2 fix: a
// card-type create (initial_status=parked) must never be treated as a
// launcher's continuation, even when the caller owns the card_requests row —
// only execution-type tasks are valid continuations.
func TestBoidOpTaskCreate_CardTypeCreate_NeverAttaches(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(conn)
	card := &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	cardReq := &orchestrator.CardRequest{
		CardID:        card.ID,
		Status:        orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-launcher",
	}
	if err := orchestrator.CreateCardRequest(conn, cardReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}

	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{
			Tasks:             repo,
			CardRequestLinker: repo,
			Meta:              executorMetaStub{meta: &orchestrator.ProjectMeta{}},
		},
		cardRequests: repo,
	}
	ctx := sandbox.TokenContext{
		ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"},
		JobID: "job-launcher", CardID: card.ID, CardRequestID: cardReq.ID,
	}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"a new card","initial_status":"parked"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	got, err := repo.GetCardRequest(cardReq.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("status = %q, want unchanged launching — a card-type create must never consume the slot", got.Status)
	}
}

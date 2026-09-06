package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- the single-work-slot invariant on the direct-CreateTask write port
// (createExecutionTask, task_create.go) ----
//
// These stub-based tests wire no Transactor, so they exercise
// createExecutionTask's non-atomic fallback check (cardSlotConflictWithRequests
// called on its own, before the INSERT) rather than the atomicCardCheck
// branch wire.go wires in production — see
// task_create_card_slot_atomic_test.go for that branch's own coverage
// against a real DB. The underlying conflict logic (cardSlotConflictWithLister)
// is shared by both, so these still pin it correctly; they just don't prove
// the atomic branch specifically.
//
// A Go accept (acceptGo, workflow_card.go) ALSO funnels through this same
// invariant — its own dedicated tests (accept_go_test.go) exercise it via a
// fake TaskCreator that bypasses this real code path entirely, so the
// fulfill-the-reservation exception below needs its own coverage here
// against the actual TaskAppService.CreateTask.

func cardParentWithDetail(id string, detail []byte, openChildCount int) *orchestrator.Task {
	return &orchestrator.Task{
		ID:             id,
		Type:           orchestrator.TaskTypeCard,
		Status:         orchestrator.TaskStatusWorking,
		Card:           &orchestrator.CardAttrs{TaskID: id, Detail: detail},
		OpenChildCount: openChildCount,
	}
}

func TestCreateTask_RejectsWhenCardSlotOccupiedByOpenChild(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"open"}]}`), 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "some-other-id",
	})
	if err == nil {
		t.Fatal("expected rejection creating a second child under a card whose slot is occupied")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.createdTask != nil {
		t.Fatal("must not have inserted a task row")
	}
}

func TestCreateTask_RejectsWhenCardSlotOccupiedByLiveTaskRow(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 1) // live row, no JSON child at all
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "some-other-id",
	})
	if err == nil {
		t.Fatal("expected rejection creating a child while a live task row already occupies the slot")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// TestCreateTask_AllowsFulfillingTheSpeccedChildsOwnReservation pins that
// acceptGo's own CreateTask call (Ref: children[i].ID, workflow_card.go)
// is NOT treated as a new occupant — it is completing the reservation the
// specced JSON child itself already holds.
func TestCreateTask_AllowsFulfillingTheSpeccedChildsOwnReservation(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`), 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "do it",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "ch_00",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (fulfills ch_00's own reservation)", err)
	}
	if got == nil || store.createdTask == nil {
		t.Fatal("expected a task to have been created")
	}
}

// TestCreateTask_RejectsRefMatchWithMismatchedProjectOrBehavior closes the
// "own reservation" exception's own spoofing hole: matching the occupant's
// id by Ref ALONE is not enough, since Ref is fully caller-controlled and
// the occupant id is readable from the card's own detail — a caller could
// otherwise plant an UNRELATED task (wrong project/behavior/instructions)
// under a matching ref, which a later legitimate acceptGo call would then
// adopt via FindTaskByRef's own get-or-create instead of creating the
// actually-specced work. Requiring project+behavior to match what
// child_specced itself recorded closes this without new plumbing.
func TestCreateTask_RejectsRefMatchWithMismatchedProjectOrBehavior(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`), 0)
	cases := []struct {
		name      string
		projectID string
		behavior  string
	}{
		{"wrong project", "proj-attacker", "dev"},
		{"wrong behavior", "proj-1", "attacker-behavior"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &stubTaskStore{
				tasks:    map[string]*orchestrator.Task{"card-1": parent},
				refTasks: map[string]*orchestrator.Task{},
			}
			svc := &TaskAppService{
				Tasks: store,
				Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}, "attacker-behavior": {}}}},
			}
			_, err := svc.CreateTask(CreateTaskRequest{
				ProjectID: c.projectID,
				Title:     "planted",
				Behavior:  c.behavior,
				ParentID:  "card-1",
				Ref:       "ch_00",
			})
			if err == nil {
				t.Fatal("expected rejection: ref matches the occupant but project/behavior do not match its own spec")
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusConflict {
				t.Fatalf("expected 409 StatusError, got %v", err)
			}
			if store.createdTask != nil {
				t.Fatal("must not have inserted a task row")
			}
		})
	}
}

// fakeCardCommandLauncherStore is a minimal CardCommandLauncherStore fake
// for pinning cardSlotConflictWithRequests' own check: countActive
// simulates that many active (launching) card_requests rows with no
// live/JSON child at all — the gap cardChildSlotConflict's own child-based
// check cannot see on its own. activeRows lets a test control exact row ids
// (e.g. to test the "fulfilling its own reservation" exclusion).
type fakeCardCommandLauncherStore struct {
	countActive int
	activeRows  []*orchestrator.CardRequest
}

func (f *fakeCardCommandLauncherStore) rows() []*orchestrator.CardRequest {
	if len(f.activeRows) > 0 {
		return f.activeRows
	}
	rows := make([]*orchestrator.CardRequest, 0, f.countActive)
	for i := 0; i < f.countActive; i++ {
		rows = append(rows, &orchestrator.CardRequest{ID: fmt.Sprintf("synthetic-%d", i), Status: orchestrator.CardRequestStatusLaunching})
	}
	return rows
}

func (f *fakeCardCommandLauncherStore) CountActiveCardRequests(cardID string) (int, error) {
	return len(f.rows()), nil
}
func (f *fakeCardCommandLauncherStore) ListCardRequestsByCard(cardID string) ([]*orchestrator.CardRequest, error) {
	return f.rows(), nil
}
func (f *fakeCardCommandLauncherStore) CreateCardRequest(req *orchestrator.CardRequest) error {
	return nil
}
func (f *fakeCardCommandLauncherStore) FailCardRequest(id, errText string) error { return nil }

// TestCreateTask_RejectsWhenActiveCardRequestOccupiesSlot pins that a
// direct `--parent <card>` create (task_create.go) must also see an active
// card_requests row (a command launcher or a Go reservation that has
// claimed the slot but not yet created its own continuation) as an
// occupant, even when no live/JSON child exists yet at all.
func TestCreateTask_RejectsWhenActiveCardRequestOccupiesSlot(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0) // no live/JSON child at all
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks:        store,
		Meta:         stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequests: &fakeCardCommandLauncherStore{countActive: 1},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "some-other-id",
	})
	if err == nil {
		t.Fatal("expected rejection creating a direct child while an active card_requests row occupies the slot")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.createdTask != nil {
		t.Fatal("must not have inserted a task row")
	}
}

// TestCreateTask_AllowsFulfillingItsOwnCardRequestReservation pins that
// acceptGo's own flow — reserve a card_requests row, then CreateTask with
// CardRequestID set to that SAME reservation — must not self-conflict: the
// row it just created is not a NEW occupant, it's the reservation this very
// call is fulfilling.
func TestCreateTask_AllowsFulfillingItsOwnCardRequestReservation(t *testing.T) {
	detail := []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`)
	parent := cardParentWithDetail("card-1", detail, 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequests: &fakeCardCommandLauncherStore{activeRows: []*orchestrator.CardRequest{
			{ID: "req-1", Status: orchestrator.CardRequestStatusLaunching},
		}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "next",
		Behavior:      "dev",
		ParentID:      "card-1",
		Ref:           "ch_00",
		CardRequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (fulfills its own reservation req-1)", err)
	}
}

// TestCreateTask_RejectsWhenADifferentCardRequestIsActive pins that the
// exclusion above is narrow: an active row that does NOT match the caller's
// own CardRequestID must still block, even when the caller carries some
// (unrelated) CardRequestID of its own.
func TestCreateTask_RejectsWhenADifferentCardRequestIsActive(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequests: &fakeCardCommandLauncherStore{activeRows: []*orchestrator.CardRequest{
			{ID: "someone-elses-request", Status: orchestrator.CardRequestStatusAttached},
		}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "a new child",
		Behavior:      "dev",
		ParentID:      "card-1",
		Ref:           "some-other-id",
		CardRequestID: "req-1",
	})
	if err == nil {
		t.Fatal("expected rejection: the active row belongs to a different request")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// TestCreateTask_AllowsWhenCardHasNoActiveRequestAndNilCardRequestsStore
// pins the nil-tolerant posture (CardRequests unset — old callers/tests that
// never wire it): the additional check must simply be skipped, not panic or
// reject.
func TestCreateTask_AllowsWhenCardHasNoActiveRequestAndNilCardRequestsStore(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		// CardRequests deliberately left nil.
	}

	if _, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "some-id",
	}); err != nil {
		t.Fatalf("CreateTask() error = %v, want success (nil CardRequests store must not block creates)", err)
	}
}

// TestCreateTask_AllowsWhenCardHasNoOpenSlot is the sanity regression: a
// card with nothing occupying its slot must let a fresh child through
// normally.
func TestCreateTask_AllowsWhenCardHasNoOpenSlot(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "first child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "ch_00",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (empty slot)", err)
	}
	if got == nil {
		t.Fatal("expected a task to have been created")
	}
}

// TestCreateTask_IdempotencyKeyRetry_ReturnsExistingChild_EvenWhenSlotOccupied
// pins that IdempotencyKey's get-or-create runs at the SERVICE layer, before
// the card slot check — same as Ref's already does — so a retry with no ref
// (idempotency_key only) against a slot its OWN earlier child already
// occupies returns the existing task instead of a false 409 "slot occupied".
func TestCreateTask_IdempotencyKeyRetry_ReturnsExistingChild_EvenWhenSlotOccupied(t *testing.T) {
	existingChild := &orchestrator.Task{
		ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: "card-1",
		Status: orchestrator.TaskStatusExecuting, IdempotencyKey: "req-1",
	}
	parent := cardParentWithDetail("card-1", nil, 1) // existingChild itself is the live occupant
	store := &stubTaskStore{
		tasks:            map[string]*orchestrator.Task{"card-1": parent, "child-1": existingChild},
		refTasks:         map[string]*orchestrator.Task{},
		idempotencyTasks: map[string]*orchestrator.Task{"proj-1:card-1:req-1": existingChild},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:      "proj-1",
		Title:          "retry",
		Behavior:       "dev",
		ParentID:       "card-1",
		IdempotencyKey: "req-1",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (idempotency get-or-create must win over the slot check)", err)
	}
	if got == nil || got.ID != "child-1" {
		t.Fatalf("got = %+v, want the existing child-1 returned, not rejected or re-created", got)
	}
	if store.createdTask != nil {
		t.Fatal("must not have inserted a new task row")
	}
}

// TestCreateTask_IdempotencyKeyTypeMismatch_Rejected is a follow-up
// regression to the retry test above: the service-layer IdempotencyKey
// get-or-create it introduces short-circuits BEFORE the store layer's own
// rejectIdempotencyKeyTypeMismatch guard ever runs (orchestrator/store.go),
// so a hit against a task of the WRONG type must be caught here too — an
// execution-task create that hits a card's idempotency_key (or vice versa)
// must error, not silently hand back the wrong-shaped task with 200.
func TestCreateTask_IdempotencyKeyTypeMismatch_Rejected(t *testing.T) {
	existingCard := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	store := &stubTaskStore{
		tasks:            map[string]*orchestrator.Task{"card-1": existingCard},
		refTasks:         map[string]*orchestrator.Task{},
		idempotencyTasks: map[string]*orchestrator.Task{"proj-1::K": existingCard},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:      "proj-1",
		Title:          "an execution task",
		Behavior:       "dev",
		IdempotencyKey: "K",
	})
	if err == nil {
		t.Fatal("expected rejection: idempotency_key already used by a different task type (card)")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
	if store.createdTask != nil {
		t.Fatal("must not have inserted a task row")
	}
}

// TestCreateTask_RejectsWhenMultipleOccupants_RegardlessOfListOrder pins
// that cardChildSlotConflict scans every occupant rather than stopping at
// the first open/specced entry: a create fulfilling one of two occupants'
// own reservation must be rejected regardless of which occupant a naive
// scan would see first — only exactly ONE occupant total, matching the
// reservation being fulfilled, lets a create through.
func TestCreateTask_RejectsWhenMultipleOccupants_RegardlessOfListOrder(t *testing.T) {
	specDetail := `{"id":"ch_01","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}`
	openDetail := `{"id":"ch_00","status":"open"}`
	cases := []struct {
		name   string
		detail string
	}{
		{"open listed before specced", "[" + openDetail + "," + specDetail + "]"},
		{"specced listed before open", "[" + specDetail + "," + openDetail + "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := cardParentWithDetail("card-1", []byte(`{"children":`+c.detail+`}`), 0)
			store := &stubTaskStore{
				tasks:    map[string]*orchestrator.Task{"card-1": parent},
				refTasks: map[string]*orchestrator.Task{},
			}
			svc := &TaskAppService{
				Tasks: store,
				Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
			}

			// Attempt to fulfill ch_01's own reservation — must still be
			// rejected because ch_00 (open) is a second, unresolved occupant.
			_, err := svc.CreateTask(CreateTaskRequest{
				ProjectID: "proj-1",
				Title:     "do it",
				Behavior:  "dev",
				ParentID:  "card-1",
				Ref:       "ch_01",
			})
			if err == nil {
				t.Fatal("expected rejection: a second unresolved occupant (open sibling) must block Go regardless of list order")
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusConflict {
				t.Fatalf("expected 409 StatusError, got %v", err)
			}
			if store.createdTask != nil {
				t.Fatal("must not have inserted a task row")
			}
		})
	}
}

// ---- UpdateTask's own reparenting write port ----
//
// `boid task update <id> --parent-id <card-id>` reparents an EXISTING task —
// neither the child_added gate nor createExecutionTask's own gate runs at
// update time, so this is a distinct write port the invariant audit must
// cover separately.

func TestUpdateTask_RejectsReparentingWhenCardSlotOccupied(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"open"}]}`), 0)
	existing := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  existing,
		tasks: map[string]*orchestrator.Task{"t1": existing, "card-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	newParent := "card-1"
	_, err := svc.UpdateTask("t1", UpdateTaskRequest{ParentID: &newParent})
	if err == nil {
		t.Fatal("expected rejection reparenting under a card whose slot is occupied")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("UpdateTask store call count = %d, want 0", store.updateCalls)
	}
}

func TestUpdateTask_AllowsReparentingWhenCardSlotIsFree(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	existing := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  existing,
		tasks: map[string]*orchestrator.Task{"t1": existing, "card-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	newParent := "card-1"
	if _, err := svc.UpdateTask("t1", UpdateTaskRequest{ParentID: &newParent}); err != nil {
		t.Fatalf("UpdateTask() error = %v, want success (empty slot)", err)
	}
	if store.updateCalls != 1 {
		t.Errorf("UpdateTask store call count = %d, want 1", store.updateCalls)
	}
}

func TestUpdateTask_ExecutionParentReparent_NotGated(t *testing.T) {
	parent := &orchestrator.Task{ID: "supervisor-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "supervisor"}, OpenChildCount: 1}
	existing := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  existing,
		tasks: map[string]*orchestrator.Task{"t1": existing, "supervisor-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	newParent := "supervisor-1"
	if _, err := svc.UpdateTask("t1", UpdateTaskRequest{ParentID: &newParent}); err != nil {
		t.Fatalf("UpdateTask() error = %v, want success (execution parents are not slot-gated)", err)
	}
}

// ---- RerunTask's own write port ----
//
// `boid task rerun <id>` resets a done/aborted execution task back to
// pending — the identical "terminal child → non-terminal under a card
// parent" move reopen is gated for, and reachable from the Web UI's Rerun
// button rendered right next to Reopen on every done/aborted task detail
// page (tasks.templ).

func TestRerunTask_RejectsWhenCardSlotOccupiedByAnotherLiveChild(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 1) // a different live sibling
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", ParentID: "card-1", Ref: "ch_00", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  task,
		tasks: map[string]*orchestrator.Task{"t1": task, "card-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	_, err := svc.RerunTask("t1", RerunTaskRequest{})
	if err == nil {
		t.Fatal("expected rejection rerunning a child while the card's slot is occupied by another live child")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("UpdateTask store call count = %d, want 0", store.updateCalls)
	}
}

func TestRerunTask_AllowsWhenCardSlotIsFree(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", ParentID: "card-1", Ref: "ch_00", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  task,
		tasks: map[string]*orchestrator.Task{"t1": task, "card-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	if _, err := svc.RerunTask("t1", RerunTaskRequest{}); err != nil {
		t.Fatalf("RerunTask() error = %v, want success (empty slot)", err)
	}
	if task.Status != orchestrator.TaskStatusPending {
		t.Errorf("status = %q, want pending", task.Status)
	}
}

// ---- createCardTask's own write port ----
//
// A card-type child has no legitimate "fulfilling a specced reservation"
// story — acceptGo only ever dispatches execution tasks — so any occupant
// unconditionally blocks creating a card under a card.

func TestCreateTask_RejectsCardTypeChildWhenCardSlotOccupied(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"open"}]}`), 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{Tasks: store}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "nested card",
		ParentID:      "card-1",
		InitialStatus: "parked",
	})
	if err == nil {
		t.Fatal("expected rejection creating a card-type child while the parent card's slot is occupied")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.createdTask != nil {
		t.Fatal("must not have inserted a task row")
	}
}

// TestCreateTask_ExecutionParent_NotGated pins that the single-work-slot
// limit applies only directly under a card, never under an execution
// parent: a supervisor (execution-type parent) with an already non-terminal
// child must NOT block a second, parallel child.
func TestCreateTask_ExecutionParent_NotGated(t *testing.T) {
	parent := &orchestrator.Task{
		ID:             "supervisor-1",
		Type:           orchestrator.TaskTypeExecution,
		Status:         orchestrator.TaskStatusExecuting,
		Exec:           &orchestrator.ExecAttrs{Behavior: "supervisor"},
		OpenChildCount: 1,
	}
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"supervisor-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "parallel child #2",
		Behavior:  "dev",
		ParentID:  "supervisor-1",
		Ref:       "child-2",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (execution parents are not slot-gated)", err)
	}
	if got == nil {
		t.Fatal("expected a task to have been created")
	}
}

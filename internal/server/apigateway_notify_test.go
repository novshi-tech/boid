package server

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

// newAPIGatewayNotifyTestDB mirrors newSecretTestDB (api_store_test.go):
// testutil.NewTestDB can't be used from inside package server (import
// cycle — testutil imports internal/server).
func newAPIGatewayNotifyTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestNewAPIGatewayRecorder_RecordsActionWithExpectedPayload pins
// docs/plans/api-gateway.md §論点3 ("確定: method + service + path + status
// を timeline に。body は記録しない") at the one hop none of
// internal/apigateway's own tests can reach: the actual orchestrator.Action
// row newAPIGatewayRecorder produces from a real TaskRepository.
func TestNewAPIGatewayRecorder_RecordsActionWithExpectedPayload(t *testing.T) {
	d := newAPIGatewayNotifyTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	tasks := orchestrator.NewTaskRepository(d.Conn)
	recorder := newAPIGatewayRecorder(tasks)
	recorder(task.ID, "GET", "myapp", "/v1/users", 200)

	actions, err := tasks.ListActionsByTask(task.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("len(actions) = %d, want 1", len(actions))
	}
	a := actions[0]
	if a.Type != timeline.ActionTypeAPIGatewayRequest {
		t.Errorf("action.Type = %q, want %q", a.Type, timeline.ActionTypeAPIGatewayRequest)
	}
	// FromStatus/ToStatus must both be set to the task's current status
	// (task_notify.go's progress-mode Action precedent) — NOT left at their
	// zero value. timeline.Build no longer surfaces these rows at all (see
	// ActionTypeAPIGatewayRequest's doc comment), but the status is still
	// meaningful audit context, so the recorder keeps stamping it.
	if a.FromStatus != task.Status || a.ToStatus != task.Status {
		t.Errorf("action.FromStatus/ToStatus = %q/%q, want both %q", a.FromStatus, a.ToStatus, task.Status)
	}
	var payload apiGatewayActionPayload
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	want := apiGatewayActionPayload{Method: "GET", Service: "myapp", Path: "/v1/users", Status: 200}
	if payload != want {
		t.Errorf("payload = %+v, want %+v", payload, want)
	}

	// The payload must never carry a request/response body — assert the
	// raw JSON has exactly the four expected keys, not merely that the
	// typed struct round-trips (which would not catch an extra field the
	// struct itself doesn't declare but some future edit might add via a
	// shared/embedded type).
	var raw map[string]any
	if err := json.Unmarshal(a.Payload, &raw); err != nil {
		t.Fatalf("unmarshal raw payload: %v", err)
	}
	if len(raw) != 4 {
		t.Errorf("payload has %d keys (%v), want exactly method/service/path/status", len(raw), raw)
	}
}

// TestNewAPIGatewayRecorder_SkipsWhenTaskIDEmpty pins the taskless-job
// contract: a taskless job (`boid exec`) has no task action log to attach
// to, so the recorder must silently skip rather than erroring or attaching
// the row to nothing.
func TestNewAPIGatewayRecorder_SkipsWhenTaskIDEmpty(t *testing.T) {
	d := newAPIGatewayNotifyTestDB(t)
	tasks := orchestrator.NewTaskRepository(d.Conn)
	recorder := newAPIGatewayRecorder(tasks)

	// Must not panic and must not attempt an insert with an empty task_id
	// (which would either violate the FK constraint or silently attach to
	// nothing) — this call is the whole assertion.
	recorder("", "GET", "myapp", "/v1/users", 200)
}

// TestNewAPIGatewayRecorder_NilTaskRepositoryIsNoop guards the construction
// site: newAPIGatewayRecorder(nil) (should never happen in production —
// wire.go always passes the real taskRepo) must not panic either.
func TestNewAPIGatewayRecorder_NilTaskRepositoryIsNoop(t *testing.T) {
	recorder := newAPIGatewayRecorder(nil)
	recorder("some-task-id", "GET", "myapp", "/v1/users", 200)
}

// TestNewAPIGatewayRecorder_UnknownTaskIDSkipsWithoutPanicking covers a
// taskID that does not resolve to any row (a stale/forged value; should not
// happen in production since Registry.Entry.TaskID always comes from a real
// dispatch, but defensive nonetheless): GetTask fails, and the recorder must
// skip rather than write an Action with an empty FromStatus/ToStatus (which
// would silently reintroduce the timeline-visibility gap this file's other
// tests guard against).
func TestNewAPIGatewayRecorder_UnknownTaskIDSkipsWithoutPanicking(t *testing.T) {
	d := newAPIGatewayNotifyTestDB(t)
	tasks := orchestrator.NewTaskRepository(d.Conn)
	recorder := newAPIGatewayRecorder(tasks)

	recorder("no-such-task-id", "GET", "myapp", "/v1/users", 200)

	actions, err := tasks.ListActionsByTask("no-such-task-id")
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("len(actions) = %d, want 0 (recording should have been skipped)", len(actions))
	}
}

// TestNewAPIGatewayRecorder_ActionRecordedButExcludedFromTimeline pins the
// revised contract (per user feedback 2026-08-07: gateway requests belong in
// the audit trail, not the noisy per-task timeline — a task that fans out
// into a paginated Jira search or a status-polling loop was drowning its
// timeline in dozens of meaningless "api: GET ... → 200" rows). The row must
// still land in the actions table via ListActionsByTask (the audit trail
// survives untouched), but timeline.Build — the sole function that turns
// []*orchestrator.Action into what the Web UI/TUI task-detail page renders
// — must NOT surface it as an Event. See timeline.go's Build/
// IsAPIGatewayRequestAction doc comments for the display-vs-storage split
// this test guards.
func TestNewAPIGatewayRecorder_ActionRecordedButExcludedFromTimeline(t *testing.T) {
	d := newAPIGatewayNotifyTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	tasks := orchestrator.NewTaskRepository(d.Conn)
	recorder := newAPIGatewayRecorder(tasks)
	recorder(task.ID, "GET", "myapp", "/v1/users", 200)

	// Positive control: record an ordinary progress Action too, so this test
	// proves the exclusion is *selective* (only api_gateway_request rows
	// drop out) rather than passing vacuously if Build() dropped every
	// Action. Without this, a regression that made Build() discard all
	// actions would still pass the assertions below.
	if err := tasks.CreateAction(&orchestrator.Action{
		TaskID:     task.ID,
		Type:       "progress",
		Payload:    []byte(`{"message":"control row"}`),
		FromStatus: task.Status,
		ToStatus:   task.Status,
	}); err != nil {
		t.Fatalf("create control progress action: %v", err)
	}

	// Audit trail: both rows must still exist in the actions table.
	actions, err := tasks.ListActionsByTask(task.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2 (audit trail must be preserved for both rows)", len(actions))
	}
	var gatewayRow *orchestrator.Action
	for _, a := range actions {
		if a.Type == timeline.ActionTypeAPIGatewayRequest {
			gatewayRow = a
		}
	}
	if gatewayRow == nil {
		t.Fatalf("no action with Type = %q found among recorded actions (actions=%+v)", timeline.ActionTypeAPIGatewayRequest, actions)
	}

	// Timeline display: the gateway row must NOT appear as a rendered
	// Event, but the control progress row must.
	groups := timeline.Build(task, actions, nil)
	var sawGateway, sawControl bool
	for i := range groups {
		for j := range groups[i].Events {
			ev := &groups[i].Events[j]
			if ev.Kind != timeline.KindAction || ev.Action == nil {
				continue
			}
			switch ev.Action.Type {
			case timeline.ActionTypeAPIGatewayRequest:
				sawGateway = true
			case "progress":
				sawControl = true
			}
		}
	}
	if sawGateway {
		t.Errorf("api gateway request action leaked into rendered timeline — it must stay audit-only")
	}
	if !sawControl {
		t.Errorf("control progress action was excluded too — Build() must only drop api_gateway_request rows, not all actions")
	}
}

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
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
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
	// zero value — or timeline.Build opens a spurious empty-status group
	// instead of placing this action in the task's real current group,
	// silently hiding it from the rendered timeline. See
	// TestNewAPIGatewayRecorder_ActionIsVisibleInTimeline for the
	// end-to-end proof.
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

// TestNewAPIGatewayRecorder_ActionIsVisibleInTimeline is the end-to-end
// proof for docs/plans/api-gateway.md §論点3's actual user-visible claim:
// not merely that a row lands in the actions table (the other tests in this
// file), but that timeline.Build — the sole function that turns
// []*orchestrator.Action into what the Web UI's task-detail page renders
// (internal/api/web.go) — actually includes it. An earlier version of
// newAPIGatewayRecorder left FromStatus/ToStatus at their zero value, which
// timeline.Build's per-item group-placement logic (it reads a.FromStatus
// unconditionally for every non-job item) interpreted as "this action
// belongs to an empty-string status group" — a group nothing else ever
// renders into or navigates to. The row existed in the DB and every other
// test in this file passed, but the feature this PR describes ("recorded to
// the timeline") was invisible to an actual viewer. This test would have
// failed against that version and is the regression guard for it.
func TestNewAPIGatewayRecorder_ActionIsVisibleInTimeline(t *testing.T) {
	d := newAPIGatewayNotifyTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
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

	groups := timeline.Build(task, actions, nil)
	var found *timeline.Event
	for i := range groups {
		for j := range groups[i].Events {
			ev := &groups[i].Events[j]
			if ev.Kind == timeline.KindAction && ev.Action != nil && ev.Action.Type == timeline.ActionTypeAPIGatewayRequest {
				found = ev
			}
		}
	}
	if found == nil {
		t.Fatalf("api gateway request action was not present in any rendered timeline group (groups=%+v)", groups)
	}
	wantLabel := "api: GET myapp/v1/users → 200"
	if found.Label != wantLabel {
		t.Errorf("timeline label = %q, want %q", found.Label, wantLabel)
	}
}

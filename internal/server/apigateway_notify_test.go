package server

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
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
	if a.Type != apiGatewayActionType {
		t.Errorf("action.Type = %q, want %q", a.Type, apiGatewayActionType)
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

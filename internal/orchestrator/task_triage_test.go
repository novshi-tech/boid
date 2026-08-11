package orchestrator_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTestDB opens an in-memory DB via db.Open (not a bare sql.Open) so
// PRAGMA foreign_keys=ON is set — required for task_triage's
// ON DELETE CASCADE to actually fire (see
// TestTaskTriage_DeleteAndCascadeOnTaskDelete).
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return d.Conn
}

func createTestTask(t *testing.T, dbtx interface {
	Exec(string, ...any) (sql.Result, error)
}, id string, status orchestrator.TaskStatus) {
	t.Helper()
	now := time.Now().UTC()
	_, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"proj-"+id, "/tmp/"+id, now, now,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_, err = dbtx.Exec(
		`INSERT INTO tasks (id, project_id, title, status, behavior, payload, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, '{}', ?, ?)`,
		id, "proj-"+id, "task "+id, string(status), "dev", now, now,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func TestTaskTriage_UpsertAndGet(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusTriaged)

	wakeAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tt := &orchestrator.TaskTriage{
		TaskID:  "t1",
		Kind:    "issue",
		Urgency: "today",
		WakeAt:  &wakeAt,
		Detail:  []byte(`{"summary":"test"}`),
	}
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}

	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if got.Kind != "issue" || got.Urgency != "today" {
		t.Fatalf("got %+v", got)
	}
	if got.WakeAt == nil || !got.WakeAt.Equal(wakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wakeAt)
	}
	if string(got.Detail) != `{"summary":"test"}` {
		t.Fatalf("Detail = %s", got.Detail)
	}
}

func TestTaskTriage_UpsertUpdatesExistingRow(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusTriaged)

	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.TaskTriage{TaskID: "t1", Urgency: "week"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.TaskTriage{TaskID: "t1", Urgency: "now"}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if got.Urgency != "now" {
		t.Fatalf("Urgency = %q, want %q (upsert should update, not duplicate)", got.Urgency, "now")
	}
}

func TestTaskTriage_GetNotFound(t *testing.T) {
	conn := newTestDB(t)
	if _, err := orchestrator.GetTaskTriage(conn, "missing"); err == nil {
		t.Fatal("expected error for missing task_triage row")
	}
}

func TestTaskTriage_DeleteAndCascadeOnTaskDelete(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusTriaged)
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.TaskTriage{TaskID: "t1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// FK ON DELETE CASCADE: deleting the task row must delete the sidecar row too
	// (Opus指摘#14 — this is the behavior GC's bare `DELETE FROM tasks` relies on).
	if _, err := conn.Exec(`DELETE FROM tasks WHERE id = ?`, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := orchestrator.GetTaskTriage(conn, "t1"); err == nil {
		t.Fatal("expected task_triage row to be cascade-deleted with its task")
	}
}

func TestTaskTriage_ExplicitDelete(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusTriaged)
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.TaskTriage{TaskID: "t1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := orchestrator.DeleteTaskTriage(conn, "t1"); err != nil {
		t.Fatalf("DeleteTaskTriage: %v", err)
	}
	if _, err := orchestrator.GetTaskTriage(conn, "t1"); err == nil {
		t.Fatal("expected error after explicit delete")
	}
}

// ParkedFrom は actions ログ (from_status) から park 直前の状態を導出する。
// task_triage に parked_from 列を重複保存しない設計 (Opus指摘#1、決定13準拠)。
func TestParkedFrom_DerivesFromLatestParkAction(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	if err := orchestrator.CreateAction(conn, &orchestrator.Action{
		TaskID: "t1", Type: "park", FromStatus: orchestrator.TaskStatusTriaged, ToStatus: orchestrator.TaskStatusParked,
	}); err != nil {
		t.Fatalf("create park action: %v", err)
	}

	from, err := orchestrator.ParkedFrom(conn, "t1")
	if err != nil {
		t.Fatalf("ParkedFrom: %v", err)
	}
	if from != orchestrator.TaskStatusTriaged {
		t.Fatalf("ParkedFrom = %q, want %q", from, orchestrator.TaskStatusTriaged)
	}
}

func TestParkedFrom_UsesMostRecentParkAction(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	// triaged -> ready -> park(from ready) -> wake -> ... -> park(from triaged) again.
	actions := []struct {
		typ  string
		from orchestrator.TaskStatus
		to   orchestrator.TaskStatus
	}{
		{"ready", orchestrator.TaskStatusTriaged, orchestrator.TaskStatusReady},
		{"park", orchestrator.TaskStatusReady, orchestrator.TaskStatusParked},
		{"wake_ready", orchestrator.TaskStatusParked, orchestrator.TaskStatusReady},
		{"park", orchestrator.TaskStatusReady, orchestrator.TaskStatusParked},
		{"wake_ready", orchestrator.TaskStatusParked, orchestrator.TaskStatusReady},
		{"triage", orchestrator.TaskStatusReady, orchestrator.TaskStatusTriaged}, // won't happen in practice, just to vary from_status
		{"park", orchestrator.TaskStatusTriaged, orchestrator.TaskStatusParked},
	}
	for i, a := range actions {
		time.Sleep(time.Millisecond) // ensure created_at ordering is stable
		if err := orchestrator.CreateAction(conn, &orchestrator.Action{
			TaskID: "t1", Type: a.typ, FromStatus: a.from, ToStatus: a.to,
		}); err != nil {
			t.Fatalf("create action %d: %v", i, err)
		}
	}

	from, err := orchestrator.ParkedFrom(conn, "t1")
	if err != nil {
		t.Fatalf("ParkedFrom: %v", err)
	}
	if from != orchestrator.TaskStatusTriaged {
		t.Fatalf("ParkedFrom = %q, want %q (most recent park action)", from, orchestrator.TaskStatusTriaged)
	}
}

func TestParkedFrom_NoParkAction(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusTriaged)
	if _, err := orchestrator.ParkedFrom(conn, "t1"); err == nil {
		t.Fatal("expected error when no park action exists")
	}
}

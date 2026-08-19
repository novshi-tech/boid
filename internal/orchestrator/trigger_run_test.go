package orchestrator_test

// docs/plans/ingestion-identity.md PR-4 (B-5): trigger_runs 台帳 (migration
// 0043) の store 層テスト。「検証」節を直接 pin する:
//   - single-flight: 同じ (project_id, trigger_name) に finished_at IS NULL
//     の行がある間は ListInFlightTriggerRuns がそれを返し続ける
//   - CompleteTriggerRun が exit_code を含めて記録する (非ゼロでも記録できる
//     — 「コマンドが非ゼロで落ちても…exit code が記録に残る」)
//   - LatestTriggerRun は最新の started_at を返す (elapsed 判定の材料)
//   - project 削除で trigger_runs 行が cascade で消える

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestCreateTriggerRun_AssignsIDWhenEmpty(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "job-1", StartedAt: time.Now().UTC()}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("CreateTriggerRun: %v", err)
	}
	if run.ID == "" {
		t.Error("CreateTriggerRun did not assign an ID")
	}
}

func TestListInFlightTriggerRuns_OnlyReturnsUnfinished(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()

	inFlight := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "job-1", StartedAt: now}
	if err := orchestrator.CreateTriggerRun(d.Conn, inFlight); err != nil {
		t.Fatalf("create in-flight run: %v", err)
	}
	finished := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-2", StartedAt: now}
	if err := orchestrator.CreateTriggerRun(d.Conn, finished); err != nil {
		t.Fatalf("create finished run: %v", err)
	}
	if err := orchestrator.CompleteTriggerRun(d.Conn, finished.ID, now.Add(time.Minute), 0); err != nil {
		t.Fatalf("complete run: %v", err)
	}

	runs, err := orchestrator.ListInFlightTriggerRuns(d.Conn)
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListInFlightTriggerRuns returned %d rows, want 1: %+v", len(runs), runs)
	}
	if runs[0].ID != inFlight.ID {
		t.Errorf("returned run ID = %q, want %q (the finished run must not appear)", runs[0].ID, inFlight.ID)
	}
	if runs[0].FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil for an in-flight run", runs[0].FinishedAt)
	}
}

func TestCompleteTriggerRun_RecordsNonZeroExitCode(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "job-1", StartedAt: now}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	finishedAt := now.Add(5 * time.Second)
	if err := orchestrator.CompleteTriggerRun(d.Conn, run.ID, finishedAt, 17); err != nil {
		t.Fatalf("CompleteTriggerRun: %v", err)
	}

	latest, err := orchestrator.LatestTriggerRun(d.Conn, "proj-1", "intake")
	if err != nil {
		t.Fatalf("LatestTriggerRun: %v", err)
	}
	if latest.ExitCode == nil || *latest.ExitCode != 17 {
		t.Errorf("ExitCode = %v, want 17 (a failing trigger's exit code must be recorded, not swallowed)", latest.ExitCode)
	}
	if latest.FinishedAt == nil || !latest.FinishedAt.Equal(finishedAt) {
		t.Errorf("FinishedAt = %v, want %v", latest.FinishedAt, finishedAt)
	}

	// No longer in-flight.
	runs, err := orchestrator.ListInFlightTriggerRuns(d.Conn)
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("ListInFlightTriggerRuns after completion = %+v, want empty", runs)
	}
}

func TestLatestTriggerRun_NoneYet_ReturnsSentinel(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, err := orchestrator.LatestTriggerRun(d.Conn, "proj-1", "never-run")
	if err != orchestrator.ErrTriggerRunNotFound {
		t.Fatalf("LatestTriggerRun for a never-run trigger: err = %v, want ErrTriggerRunNotFound", err)
	}
}

func TestLatestTriggerRun_ReturnsMostRecentByStartedAt(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := time.Now().UTC()

	older := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "job-old", StartedAt: base}
	if err := orchestrator.CreateTriggerRun(d.Conn, older); err != nil {
		t.Fatalf("create older run: %v", err)
	}
	if err := orchestrator.CompleteTriggerRun(d.Conn, older.ID, base.Add(time.Second), 0); err != nil {
		t.Fatalf("complete older run: %v", err)
	}
	newer := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "job-new", StartedAt: base.Add(10 * time.Minute)}
	if err := orchestrator.CreateTriggerRun(d.Conn, newer); err != nil {
		t.Fatalf("create newer run: %v", err)
	}

	latest, err := orchestrator.LatestTriggerRun(d.Conn, "proj-1", "intake")
	if err != nil {
		t.Fatalf("LatestTriggerRun: %v", err)
	}
	if latest.ID != newer.ID {
		t.Errorf("LatestTriggerRun returned %q, want the newer run %q", latest.ID, newer.ID)
	}

	// A different trigger name in the same project is scoped independently.
	if _, err := orchestrator.LatestTriggerRun(d.Conn, "proj-1", "sweep"); err != orchestrator.ErrTriggerRunNotFound {
		t.Errorf("LatestTriggerRun for a different trigger name: err = %v, want ErrTriggerRunNotFound", err)
	}
}

// TestTriggerRuns_CascadeDeletedWithProject pins this table's FK: deleting a
// project must not require a separate explicit trigger_runs cleanup (see the
// migration's own comment for why CASCADE was chosen over DeleteProject's
// manual-sweep pattern).
func TestTriggerRuns_CascadeDeletedWithProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "job-1", StartedAt: time.Now().UTC()}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := orchestrator.DeleteProject(d.Conn, "proj-1"); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	var count int
	if err := d.Conn.QueryRow("SELECT COUNT(*) FROM trigger_runs").Scan(&count); err != nil {
		t.Fatalf("count trigger_runs: %v", err)
	}
	if count != 0 {
		t.Errorf("trigger_runs rows after project delete = %d, want 0 (cascade)", count)
	}
}

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
	"errors"
	"strings"
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

// TestCreateTriggerRun_JobIDOptional pins the Opus review Blocker 1 schema
// change (migration 0043): job_id is no longer required at insert time — the
// row is created FIRST (claiming single-flight via the partial UNIQUE index)
// and job_id is filled in later, once StartExec actually returns one, via
// SetTriggerRunJobID.
func TestCreateTriggerRun_JobIDOptional(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("CreateTriggerRun with empty JobID: %v", err)
	}
	if run.JobID != "" {
		t.Errorf("JobID = %q, want empty (untouched)", run.JobID)
	}
}

// TestCreateTriggerRun_SecondInFlightForSameKey_RejectedByUniqueIndex is the
// DB-level enforcement Blocker 1 added: idx_trigger_runs_inflight_unique
// (migration 0043) rejects a second finished_at-IS-NULL row for the same
// (project_id, trigger_name), surfaced as ErrTriggerRunInFlight — this is
// what makes single-flight hold even when two callers' own in-memory
// "is anything in flight" checks both raced and both said "no".
func TestCreateTriggerRun_SecondInFlightForSameKey_RejectedByUniqueIndex(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	first := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, first); err != nil {
		t.Fatalf("create first run: %v", err)
	}

	second := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	err := orchestrator.CreateTriggerRun(d.Conn, second)
	if !errors.Is(err, orchestrator.ErrTriggerRunInFlight) {
		t.Fatalf("create second in-flight run for the same key: err = %v, want ErrTriggerRunInFlight", err)
	}

	// A DIFFERENT trigger name in the same project is unaffected (the
	// constraint is scoped per (project_id, trigger_name), not per project).
	other := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, other); err != nil {
		t.Fatalf("create run for a different trigger name: %v", err)
	}

	// Completing the first run releases the constraint — a new in-flight row
	// for "intake" can now be created.
	if err := orchestrator.CompleteTriggerRun(d.Conn, first.ID, time.Now(), 0); err != nil {
		t.Fatalf("complete first run: %v", err)
	}
	third := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, third); err != nil {
		t.Fatalf("create run after the blocking one completed: %v", err)
	}
}

// TestSetTriggerRunJobID_FillsInAfterDispatch pins the second half of the
// insert-then-dispatch split: the row is created with an empty job_id, then
// SetTriggerRunJobID records the id StartExec actually returned.
func TestSetTriggerRunJobID_FillsInAfterDispatch(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := orchestrator.SetTriggerRunJobID(d.Conn, run.ID, "job-42"); err != nil {
		t.Fatalf("SetTriggerRunJobID: %v", err)
	}
	runs, err := orchestrator.ListInFlightTriggerRuns(d.Conn)
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].JobID != "job-42" {
		t.Fatalf("runs = %+v, want exactly 1 row with JobID=job-42", runs)
	}
}

func TestSetTriggerRunJobID_UnknownID_Errors(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.SetTriggerRunJobID(d.Conn, "does-not-exist", "job-1"); err == nil {
		t.Fatal("SetTriggerRunJobID for an unknown id = nil error, want an error")
	}
}

// TestDeleteTriggerRun_ReleasesSingleFlight pins the dispatch-failure cleanup
// path (fireTrigger, Opus review Blocker 1): deleting a claimed-but-never-
// dispatched row must free the (project_id, trigger_name) slot immediately,
// matching the pre-fix "fail-open, retry the very same instant" behavior
// (TestSweepTriggers_DispatchFailure_FailsOpenAndRetriesImmediately).
func TestDeleteTriggerRun_ReleasesSingleFlight(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := orchestrator.DeleteTriggerRun(d.Conn, run.ID); err != nil {
		t.Fatalf("DeleteTriggerRun: %v", err)
	}

	runs, err := orchestrator.ListInFlightTriggerRuns(d.Conn)
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("in-flight runs after delete = %+v, want empty", runs)
	}
	// The slot is free again: a fresh in-flight row for the SAME key can be
	// created immediately, with no constraint violation.
	retry := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now()}
	if err := orchestrator.CreateTriggerRun(d.Conn, retry); err != nil {
		t.Fatalf("create run after delete: %v", err)
	}
}

// TestCreateTriggerRun_NormalizesStartedAtToUTC pins N-9 (Opus review):
// StartedAt must be stored as UTC even when the caller passes a time with a
// local/non-UTC offset, so ORDER BY started_at (a TEXT lexicographic
// comparison) stays correct regardless of the daemon's local timezone.
func TestCreateTriggerRun_NormalizesStartedAtToUTC(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	jst := time.FixedZone("JST", 9*60*60)
	local := time.Date(2026, 8, 19, 18, 26, 28, 0, jst)
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: local}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	var stored string
	if err := d.Conn.QueryRow(`SELECT started_at FROM trigger_runs WHERE id = ?`, run.ID).Scan(&stored); err != nil {
		t.Fatalf("query started_at: %v", err)
	}
	if strings.Contains(stored, "+09:00") || strings.Contains(stored, "+0900") {
		t.Errorf("stored started_at = %q, still carries the local offset — want UTC", stored)
	}

	latest, err := orchestrator.LatestTriggerRun(d.Conn, "proj-1", "intake")
	if err != nil {
		t.Fatalf("LatestTriggerRun: %v", err)
	}
	if !local.Equal(latest.StartedAt) {
		t.Errorf("round-tripped StartedAt = %v, want the same instant as %v", latest.StartedAt, local)
	}
	if _, offset := latest.StartedAt.Zone(); offset != 0 {
		t.Errorf("round-tripped StartedAt offset = %ds, want 0 (UTC) — driver returned %v", offset, latest.StartedAt)
	}
}

// TestCompleteTriggerRun_NormalizesFinishedAtToUTC is CompleteTriggerRun's
// analogous half of N-9 — the GC purge this PR adds (N-2) compares
// finished_at against a UTC cutoff, so finished_at must be stored as UTC
// too, not just started_at.
func TestCompleteTriggerRun_NormalizesFinishedAtToUTC(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", StartedAt: time.Now().UTC()}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	jst := time.FixedZone("JST", 9*60*60)
	local := time.Date(2026, 8, 19, 19, 0, 0, 0, jst)
	if err := orchestrator.CompleteTriggerRun(d.Conn, run.ID, local, 0); err != nil {
		t.Fatalf("CompleteTriggerRun: %v", err)
	}

	var stored string
	if err := d.Conn.QueryRow(`SELECT finished_at FROM trigger_runs WHERE id = ?`, run.ID).Scan(&stored); err != nil {
		t.Fatalf("query finished_at: %v", err)
	}
	if strings.Contains(stored, "+09:00") || strings.Contains(stored, "+0900") {
		t.Errorf("stored finished_at = %q, still carries the local offset — want UTC", stored)
	}
}

// TestListInFlightTriggerRuns_UsesIndexNotFullScan pins N-2 (Opus review):
// EXPLAIN QUERY PLAN for the exact query ListInFlightTriggerRuns runs every
// sweep tick must not report a full table scan or a temp-b-tree sort, now
// that migration 0043 adds idx_trigger_runs_inflight_started
// (started_at, id) WHERE finished_at IS NULL.
func TestListInFlightTriggerRuns_UsesIndexNotFullScan(t *testing.T) {
	d := testutil.NewTestDB(t)
	rows, err := d.Conn.Query(
		`EXPLAIN QUERY PLAN SELECT id, project_id, trigger_name, job_id, started_at, finished_at, exit_code
		 FROM trigger_runs WHERE finished_at IS NULL ORDER BY started_at ASC, id ASC`,
	)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain rows: %v", err)
	}

	// A bare "SCAN trigger_runs" (no "USING INDEX") means sqlite walked the
	// whole table; "USE TEMP B-TREE" means it needed a separate sort step on
	// top of that. Either would reproduce N-2's reported
	// `SCAN trigger_runs` + `USE TEMP B-TREE FOR ORDER BY`. "SCAN ... USING
	// INDEX idx_trigger_runs_inflight_started" is the wanted outcome — the
	// partial index already holds exactly the in-flight rows pre-sorted by
	// (started_at, id), so walking it in order needs no extra work even
	// though EXPLAIN QUERY PLAN still labels it "SCAN" rather than "SEARCH"
	// (there is no equality/range value to seek by; every matching row is
	// read).
	usesIndex := false
	for _, line := range plan {
		if strings.Contains(line, "USING INDEX idx_trigger_runs_inflight_started") {
			usesIndex = true
		}
		if strings.Contains(line, "TEMP B-TREE") {
			t.Errorf("query plan = %v, still needs a temp b-tree sort", plan)
		}
	}
	if !usesIndex {
		t.Errorf("query plan = %v, want it to use idx_trigger_runs_inflight_started", plan)
	}
	if len(plan) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned no rows")
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

// The row has TWO closers that do not coordinate — reconcileInFlight (job
// reached a terminal status) and the trigger-timeout path — and they overlap in
// an ordinary window: aborting the task makes the parked `boid task wait`
// return, which exits the job, which the next tick sees as terminal while the
// timeout goroutine is still inside StopJobRuntime's 30-second deadline.
// Without the `finished_at IS NULL` guard the second write silently rewrites
// the first's finished_at/exit_code and the caller believes it closed the row,
// so one round is counted twice against the fail streak.
func TestCompleteTriggerRun_SecondCloseIsRefused(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: now}
	if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	first := now.Add(5 * time.Second)
	if err := orchestrator.CompleteTriggerRun(d.Conn, run.ID, first, 1); err != nil {
		t.Fatalf("first close: %v", err)
	}

	err := orchestrator.CompleteTriggerRun(d.Conn, run.ID, first.Add(time.Minute), -3)
	if err == nil {
		t.Fatal("the second close must be refused, not silently applied")
	}
	if !errors.Is(err, orchestrator.ErrTriggerRunAlreadyFinished) {
		t.Fatalf("err = %v, want ErrTriggerRunAlreadyFinished so the loser can tell a race from a failure", err)
	}

	// The first writer's values survive.
	latest, lerr := orchestrator.LatestTriggerRun(d.Conn, "proj-1", "sweep")
	if lerr != nil {
		t.Fatalf("LatestTriggerRun: %v", lerr)
	}
	if latest.ExitCode == nil || *latest.ExitCode != 1 {
		t.Errorf("ExitCode = %v, want 1 (the first writer's) — the second close overwrote it", latest.ExitCode)
	}
	if latest.FinishedAt == nil || !latest.FinishedAt.Equal(first) {
		t.Errorf("FinishedAt = %v, want %v (the first writer's)", latest.FinishedAt, first)
	}
}

// A row that does not exist reports the same sentinel: both are "my write did
// not land", and no caller needs to tell them apart.
func TestCompleteTriggerRun_UnknownIDIsTheSameSentinel(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.CompleteTriggerRun(d.Conn, "no-such-run", time.Now().UTC(), 0)
	if !errors.Is(err, orchestrator.ErrTriggerRunAlreadyFinished) {
		t.Fatalf("err = %v, want ErrTriggerRunAlreadyFinished", err)
	}
}

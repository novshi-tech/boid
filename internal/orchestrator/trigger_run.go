package orchestrator

// docs/plans/ingestion-identity.md PR-4 (B-5): trigger_runs 台帳の store 層
// (migration 0043). daemon が持つ 3 つの持ち分のうち single-flight と
// 実行結果の記録がここに載る (J-4)。
//
//   - single-flight: 「同じ (project_id, trigger_name) に finished_at IS
//     NULL の行があれば見送る」。ListInFlightTriggerRuns はその判定材料 (読み
//     取り用の一覧) だが、**実際に enforce するのは
//     idx_trigger_runs_inflight_unique の部分 UNIQUE インデックス**
//     (migration 0043) — CreateTriggerRun がこの制約に弾かれたら
//     ErrTriggerRunInFlight を返す (Opus review Blocker 1: 列挙してから書く
//     だけの実装では列挙と書き込みの間に別プロセス/goroutine が割り込める
//     レースが実際にあった)
//   - 実行結果の記録: CreateTriggerRun (started_at のみ、job_id は空文字列
//     で開始) → SetTriggerRunJobID (dispatch 成功後に job_id を埋める) →
//     CompleteTriggerRun (finished_at + exit_code、非ゼロでも記録する)。
//     dispatch 自体が失敗したら DeleteTriggerRun で claim した行ごと消す
//   - elapsed 判定: LatestTriggerRun が (project, trigger) の直近 started_at
//     を返し、呼び出し側 (internal/api/trigger_loop.go) が
//     now.Sub(latest.StartedAt) >= every かどうかを見る
//
// 全メソッドが db.DBTX を直接取るプレーン関数である点は task_identity.go /
// action_list.go と同じ形 — internal/api の TaskWorkflowService は
// TaskRepository (repository.go) 経由の narrow interface で呼ぶ。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/db"
)

// TriggerRun is one row of the trigger_runs table — one attempted (or
// in-flight) execution of a project.yaml trigger.
type TriggerRun struct {
	ID          string
	ProjectID   string
	TriggerName string
	JobID       string
	StartedAt   time.Time
	// FinishedAt is nil while the run is in flight (single-flight's own
	// definition of "busy" — ListInFlightTriggerRuns' WHERE clause).
	FinishedAt *time.Time
	// ExitCode is nil until CompleteTriggerRun records it. A recorded
	// value can be non-zero — a failing trigger command's exit code is
	// data, not an error this store rejects (PR-4's own 検証: 「コマンドが
	// 非ゼロで落ちても…exit code が記録に残る」).
	ExitCode *int
}

// ErrTriggerRunNotFound is LatestTriggerRun's sentinel for "this
// (project_id, trigger_name) has never run" — same ErrXxxNotFound
// convention as ErrTaskNotFound (task_identity.go's ResolveIdentity uses
// the same shape).
var ErrTriggerRunNotFound = errors.New("trigger run: not found")

// ErrTriggerRunInFlight is CreateTriggerRun's sentinel for "single-flight
// already holds this (project_id, trigger_name)" — surfaced when the INSERT
// is rejected by idx_trigger_runs_inflight_unique (migration 0043, Opus
// review Blocker 1), the partial UNIQUE index on
// trigger_runs(project_id, trigger_name) WHERE finished_at IS NULL. Callers
// (internal/api/trigger_loop.go's fireTrigger, and everything that calls it)
// treat this exactly like an in-memory single-flight "busy" skip — the
// difference is this one is enforced by the DB itself, so it also catches
// races the caller's own read-then-check missed (concurrent goroutines,
// concurrent daemon processes sharing one DB file).
var ErrTriggerRunInFlight = errors.New("trigger run: already in flight (single-flight)")

// CreateTriggerRun inserts a new in-flight run (FinishedAt/ExitCode unset).
// run.ID is generated when empty, mirroring CreateTask/CreateAction
// (store.go).
//
// run.JobID may be empty (Opus review Blocker 1): the row is created FIRST,
// BEFORE dispatch, so single-flight is claimed via this INSERT succeeding —
// not via a later read. There is no job id yet at that point (StartExec
// hasn't been called); SetTriggerRunJobID fills it in once dispatch
// succeeds. See internal/api/trigger_loop.go's fireTrigger for the full
// insert → dispatch → fill-in-job-id sequence, and this PR's report
// ("single-flight の直し方") for why this ordering, not the reverse, is
// required to close the race.
//
// Returns ErrTriggerRunInFlight (not a generic error) when
// idx_trigger_runs_inflight_unique rejects the insert — i.e. some other
// row for the same (project_id, trigger_name) is already in flight.
func CreateTriggerRun(dbtx db.DBTX, run *TriggerRun) error {
	if run.ProjectID == "" {
		return fmt.Errorf("create trigger run: project id must not be empty")
	}
	if run.TriggerName == "" {
		return fmt.Errorf("create trigger run: trigger name must not be empty")
	}
	if run.ID == "" {
		run.ID = uuid.New().String()
	}
	// N-9 (Opus review): always normalize to UTC, not just when the caller
	// left StartedAt zero — previously a caller-supplied non-UTC time (e.g.
	// TriggerLoop.runOnce's plain time.Now()) was stored as-is, so
	// ORDER BY started_at (a TEXT lexicographic comparison) could put a
	// "+09:00" row out of order relative to a "Z" row once the daemon's
	// local timezone changed.
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	run.StartedAt = run.StartedAt.UTC()
	_, err := dbtx.Exec(
		`INSERT INTO trigger_runs (id, project_id, trigger_name, job_id, started_at, finished_at, exit_code)
		 VALUES (?, ?, ?, ?, ?, NULL, NULL)`,
		run.ID, run.ProjectID, run.TriggerName, run.JobID, run.StartedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrTriggerRunInFlight
		}
		return fmt.Errorf("create trigger run: %w", err)
	}
	return nil
}

// SetTriggerRunJobID records the job id StartExec returned, once dispatch
// succeeds (Opus review Blocker 1's insert-then-dispatch split —
// CreateTriggerRun's own doc comment). Errors (including "no such row") are
// returned as-is rather than swallowed; the caller (fireTrigger) treats a
// failure here as a residual, logged gap — see its own doc comment.
func SetTriggerRunJobID(dbtx db.DBTX, id, jobID string) error {
	if id == "" {
		return fmt.Errorf("set trigger run job id: id must not be empty")
	}
	res, err := dbtx.Exec(`UPDATE trigger_runs SET job_id = ? WHERE id = ?`, jobID, id)
	if err != nil {
		return fmt.Errorf("set trigger run job id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set trigger run job id: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("set trigger run job id: no row with id %q", id)
	}
	return nil
}

// DeleteTriggerRun removes a trigger_runs row outright — the dispatch-
// failure cleanup path (Opus review Blocker 1): fireTrigger claims
// single-flight by inserting a row BEFORE calling StartExec, so a dispatch
// failure must undo that claim to preserve the pre-existing "fail-open,
// retry the very same instant" contract
// (TestSweepTriggers_DispatchFailure_FailsOpenAndRetriesImmediately) —
// CLOSING the row instead (CompleteTriggerRun) would leave a real
// started_at behind, making the immediately-following retry see `every` as
// not yet elapsed. Deleting, not closing, is what keeps that test's
// behavior byte-for-byte unchanged.
func DeleteTriggerRun(dbtx db.DBTX, id string) error {
	if id == "" {
		return fmt.Errorf("delete trigger run: id must not be empty")
	}
	if _, err := dbtx.Exec(`DELETE FROM trigger_runs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete trigger run: %w", err)
	}
	return nil
}

// CompleteTriggerRun records a run's terminal outcome. Idempotent in the
// sense that re-completing an already-finished row just overwrites
// finished_at/exit_code with the new values (the trigger sweep loop only
// ever calls this once per row in normal operation, but a fail-open retry
// path — see trigger_loop.go — must not error on a second call).
func CompleteTriggerRun(dbtx db.DBTX, id string, finishedAt time.Time, exitCode int) error {
	if id == "" {
		return fmt.Errorf("complete trigger run: id must not be empty")
	}
	// N-9 (Opus review): same UTC normalization as CreateTriggerRun's
	// started_at — finished_at feeds this PR's own GC retention filter
	// (N-2, GCTriggerRuns' `finished_at < ?` cutoff), so a non-UTC value
	// here would make that comparison drift the same way started_at's
	// ordering did.
	finishedAt = finishedAt.UTC()
	// `finished_at IS NULL` makes this a claim, not a blind write: the row has
	// TWO closers that do not coordinate. reconcileInFlight closes a run whose
	// job reached a terminal status, and the trigger-timeout path
	// (api.finishTimeOutTriggerRun) closes one it is ending — and those overlap
	// in an ordinary window, because aborting the task makes the parked
	// `boid task wait` return, which exits the job, which the very next tick
	// sees as terminal while the timeout goroutine is still inside
	// StopJobRuntime's 30-second deadline. Without the guard the row is closed
	// twice with different exit codes, and the second write silently rewrites
	// finished_at/exit_code over the first.
	res, err := dbtx.Exec(
		`UPDATE trigger_runs SET finished_at = ?, exit_code = ? WHERE id = ? AND finished_at IS NULL`,
		finishedAt, exitCode, id,
	)
	if err != nil {
		return fmt.Errorf("complete trigger run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete trigger run: rows affected: %w", err)
	}
	if n == 0 {
		// Either no such row, or someone else closed it first. Both are
		// reported through ErrTriggerRunAlreadyFinished so a caller that races
		// can tell "my write did not land" from a real failure and skip
		// whatever it would have done on the strength of having closed it
		// (reporting a completion, in the timeout path's case).
		return fmt.Errorf("%w: id %q", ErrTriggerRunAlreadyFinished, id)
	}
	return nil
}

// ErrTriggerRunAlreadyFinished is returned by CompleteTriggerRun when the row
// was already closed (or does not exist) — a lost race against the other
// closer, not a failure to talk to the database.
var ErrTriggerRunAlreadyFinished = errors.New("trigger run is already finished")

// ListInFlightTriggerRuns returns every row with finished_at IS NULL, across
// every project/trigger — the single query the trigger sweep loop's
// reconciliation pass uses once per tick (avoiding one query per trigger
// definition, same "single batch read" posture as PR-3's ListActionsSince).
func ListInFlightTriggerRuns(dbtx db.DBTX) ([]*TriggerRun, error) {
	rows, err := dbtx.Query(
		`SELECT id, project_id, trigger_name, job_id, started_at, finished_at, exit_code
		 FROM trigger_runs WHERE finished_at IS NULL ORDER BY started_at ASC, id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list in-flight trigger runs: %w", err)
	}
	defer rows.Close()
	return scanTriggerRuns(rows)
}

// LatestTriggerRun returns the most recently STARTED run for
// (projectID, triggerName) — regardless of whether it has finished yet.
// Returns ErrTriggerRunNotFound when the pair has never run. Callers use
// this to compute "has `every` elapsed since the last start" (this PR's
// scheduling condition) independent of single-flight (which is decided by
// ListInFlightTriggerRuns/finished_at, not by this method).
func LatestTriggerRun(dbtx db.DBTX, projectID, triggerName string) (*TriggerRun, error) {
	if projectID == "" {
		return nil, fmt.Errorf("latest trigger run: project id must not be empty")
	}
	if triggerName == "" {
		return nil, fmt.Errorf("latest trigger run: trigger name must not be empty")
	}
	row := dbtx.QueryRow(
		`SELECT id, project_id, trigger_name, job_id, started_at, finished_at, exit_code
		 FROM trigger_runs WHERE project_id = ? AND trigger_name = ?
		 ORDER BY started_at DESC, id DESC LIMIT 1`,
		projectID, triggerName,
	)
	run, err := scanTriggerRunRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTriggerRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("latest trigger run: %w", err)
	}
	return run, nil
}

// triggerRunRowScanner is satisfied by both *sql.Row and *sql.Rows (their
// Scan methods have identical signatures), letting scanTriggerRunRow /
// scanTriggerRuns below share one field list instead of two independently
// maintained SELECT-column-order copies.
type triggerRunRowScanner interface {
	Scan(dest ...any) error
}

func scanTriggerRunRow(row triggerRunRowScanner) (*TriggerRun, error) {
	var run TriggerRun
	var finishedAt sql.NullTime
	var exitCode sql.NullInt64
	if err := row.Scan(&run.ID, &run.ProjectID, &run.TriggerName, &run.JobID, &run.StartedAt, &finishedAt, &exitCode); err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		run.FinishedAt = &t
	}
	if exitCode.Valid {
		c := int(exitCode.Int64)
		run.ExitCode = &c
	}
	return &run, nil
}

func scanTriggerRuns(rows *sql.Rows) ([]*TriggerRun, error) {
	out := []*TriggerRun{}
	for rows.Next() {
		run, err := scanTriggerRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan trigger run: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trigger runs: rows: %w", err)
	}
	return out, nil
}

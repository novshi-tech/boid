package orchestrator

// docs/plans/ingestion-identity.md PR-4 (B-5): trigger_runs 台帳の store 層
// (migration 0043). daemon が持つ 3 つの持ち分のうち single-flight と
// 実行結果の記録がここに載る (J-4)。
//
//   - single-flight: 「同じ (project_id, trigger_name) に finished_at IS
//     NULL の行があれば見送る」— ListInFlightTriggerRuns がその判定材料
//   - 実行結果の記録: CreateTriggerRun (started_at + job_id) →
//     CompleteTriggerRun (finished_at + exit_code、非ゼロでも記録する)
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

// CreateTriggerRun inserts a new in-flight run (FinishedAt/ExitCode unset).
// run.ID is generated when empty, mirroring CreateTask/CreateAction
// (store.go).
func CreateTriggerRun(dbtx db.DBTX, run *TriggerRun) error {
	if run.ProjectID == "" {
		return fmt.Errorf("create trigger run: project id must not be empty")
	}
	if run.TriggerName == "" {
		return fmt.Errorf("create trigger run: trigger name must not be empty")
	}
	if run.JobID == "" {
		return fmt.Errorf("create trigger run: job id must not be empty")
	}
	if run.ID == "" {
		run.ID = uuid.New().String()
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	_, err := dbtx.Exec(
		`INSERT INTO trigger_runs (id, project_id, trigger_name, job_id, started_at, finished_at, exit_code)
		 VALUES (?, ?, ?, ?, ?, NULL, NULL)`,
		run.ID, run.ProjectID, run.TriggerName, run.JobID, run.StartedAt,
	)
	if err != nil {
		return fmt.Errorf("create trigger run: %w", err)
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
	res, err := dbtx.Exec(
		`UPDATE trigger_runs SET finished_at = ?, exit_code = ? WHERE id = ?`,
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
		return fmt.Errorf("complete trigger run: no row with id %q", id)
	}
	return nil
}

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

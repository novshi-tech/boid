package orchestrator

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// WorkspaceWatchdog is the queue の決定論的評価 節 rule 7 (沈黙の検知) primitive:
// per-workspace timestamps of "最終 ingestion 成功" and "最終棚卸し実施". Both
// are nil when never recorded — that IS a breach for any positive threshold
// (a workspace that has never ingested anything is exactly the "沈黙" this
// rule exists to catch).
//
// This is a mechanism only (PR-3 scope, docs/plans/cross-project-issue-triage.md
// PR-3 instructions): ingestion (PR-4) does not exist yet, so nothing
// currently calls TouchWorkspaceIngestSuccess in production. The primitive
// (table + accessors + threshold evaluator) is built now so PR-4's ingestion
// task has somewhere to write, and so the queue view can already render
// guidance for last_triage_review_at (which nothing blocks starting to use
// today, unlike ingestion).
type WorkspaceWatchdog struct {
	WorkspaceID         string
	LastIngestSuccessAt *time.Time
	LastTriageReviewAt  *time.Time
}

// GetWorkspaceWatchdog returns the watchdog row for workspaceID, or a
// zero-value (both timestamps nil) when no row exists yet — a workspace with
// no recorded activity at all is a valid, meaningful state for
// WatchdogGuidance to evaluate, not an error.
func GetWorkspaceWatchdog(dbtx db.DBTX, workspaceID string) (*WorkspaceWatchdog, error) {
	row := dbtx.QueryRow(
		`SELECT last_ingest_success_at, last_triage_review_at FROM workspace_watchdog WHERE workspace_id = ?`,
		workspaceID,
	)
	var ingest, review sql.NullTime
	err := row.Scan(&ingest, &review)
	if err == sql.ErrNoRows {
		return &WorkspaceWatchdog{WorkspaceID: workspaceID}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace_watchdog %q: %w", workspaceID, err)
	}
	wd := &WorkspaceWatchdog{WorkspaceID: workspaceID}
	if ingest.Valid {
		t := ingest.Time
		wd.LastIngestSuccessAt = &t
	}
	if review.Valid {
		t := review.Time
		wd.LastTriageReviewAt = &t
	}
	return wd, nil
}

// TouchWorkspaceIngestSuccess records "最終 ingestion 成功" for workspaceID,
// preserving any existing last_triage_review_at. PR-4's ingestion task is
// the intended (not-yet-existing) caller.
func TouchWorkspaceIngestSuccess(dbtx db.DBTX, workspaceID string, at time.Time) error {
	return upsertWatchdogTimestamp(dbtx, workspaceID, "last_ingest_success_at", at)
}

// TouchWorkspaceTriageReview records "最終棚卸し実施" for workspaceID
// (UC-5's periodic テーマセッション棚卸し), preserving any existing
// last_ingest_success_at.
func TouchWorkspaceTriageReview(dbtx db.DBTX, workspaceID string, at time.Time) error {
	return upsertWatchdogTimestamp(dbtx, workspaceID, "last_triage_review_at", at)
}

func upsertWatchdogTimestamp(dbtx db.DBTX, workspaceID, column string, at time.Time) error {
	if column != "last_ingest_success_at" && column != "last_triage_review_at" {
		return fmt.Errorf("upsert workspace_watchdog: unrecognized column %q", column)
	}
	// column is one of the two fixed literals checked above, never caller
	// input, so string-building the column name here is safe (same pattern
	// as sqlStatusInList's fixed-enum concatenation in store.go).
	_, err := dbtx.Exec(
		`INSERT INTO workspace_watchdog (workspace_id, `+column+`, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(workspace_id) DO UPDATE SET
		   `+column+` = excluded.`+column+`,
		   updated_at = excluded.updated_at`,
		workspaceID, at, at,
	)
	if err != nil {
		return fmt.Errorf("upsert workspace_watchdog %q.%s: %w", workspaceID, column, err)
	}
	return nil
}

// WatchdogThresholds are the staleness thresholds rule 7 checks against.
// Deliberately not yet wired to config.yaml (no real caller/data to
// calibrate against until PR-4's ingestion lands) — DefaultWatchdogThresholds
// below is a reasonable starting point, kept as a named type so a future PR
// can thread it through config without changing WatchdogGuidance's signature.
type WatchdogThresholds struct {
	IngestStale time.Duration
	ReviewStale time.Duration
}

// DefaultWatchdogThresholds returns PR-3's starting-point thresholds: 48h
// for ingestion silence, 14 days for 棚卸し silence.
func DefaultWatchdogThresholds() WatchdogThresholds {
	return WatchdogThresholds{
		IngestStale: 48 * time.Hour,
		ReviewStale: 14 * 24 * time.Hour,
	}
}

// WatchdogGuidance evaluates queue 節 rule 7 for a single workspace: it
// returns one guidance string per breached threshold (empty when nothing is
// breached). A nil/never-recorded timestamp is always a breach — "来ない
// こと" is exactly the failure mode this rule exists to surface (人間は
// 「来ないこと」に気づけないため, queue 節 rule 7).
//
// This is the "how a guidance item would surface" half of PR-3's watchdog
// scope: it is a pure function of (now, watchdog row, thresholds) — no
// agent judgment (決定12) — that the queue view (or, later, a periodic
// notify sweep) can call directly. It does not itself persist or dedupe
// guidance items; queue 節 rule 7 says a guidance item is re-emitted "解消
// されるまで出し続ける", which a caller gets for free by simply calling this
// on every render/sweep rather than this function tracking state itself.
func WatchdogGuidance(now time.Time, workspaceID string, wd *WorkspaceWatchdog, th WatchdogThresholds) []string {
	if wd == nil {
		wd = &WorkspaceWatchdog{}
	}
	var out []string
	if th.IngestStale > 0 && watchdogBreached(now, wd.LastIngestSuccessAt, th.IngestStale) {
		out = append(out, fmt.Sprintf("workspace %q: 最終 ingestion 成功から %s 以上経過（または一度も成功していません）", workspaceID, th.IngestStale))
	}
	if th.ReviewStale > 0 && watchdogBreached(now, wd.LastTriageReviewAt, th.ReviewStale) {
		out = append(out, fmt.Sprintf("workspace %q: 最終棚卸しから %s 以上経過（または一度も実施していません）", workspaceID, th.ReviewStale))
	}
	return out
}

func watchdogBreached(now time.Time, last *time.Time, threshold time.Duration) bool {
	if last == nil {
		return true
	}
	return now.Sub(*last) >= threshold
}

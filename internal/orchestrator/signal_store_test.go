package orchestrator_test

// docs/plans/signal-ingest-detailed-design.md §2 (PR-1): signal inbox
// (signals / signal_cursors, migration 0046) の store 層テスト。
//
// signal-driven-review.md §8.1 の 2 つの不変条件をこのファイルで直接 pin
// する:
//   - dedup: 同一 (workspace, service, connector, id) の再投入は no-op
//     (採点表 Q10 — TestIngestSignals_DuplicateIsNoOp)
//   - source cursor は処理済み Signal 自身を越えて前進する。occurred_at の
//     tz オフセットが混在しても文字列比較ではなく時刻として正しく比較される
//     こと、および一度前進した cursor は絶対に後退しないことを見る
//   - 「取り込み中のまま残る終着状態を持たない」= INSERT と cursor 前進が
//     同一 tx であることの crash 耐性 (採点表 Q13 —
//     TestIngestSignals_RollbackLeavesNoPartialState /
//     TestTaskRepository_IngestSignals_PartialBatchFailureRollsBackEntirely)

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

// --- IngestSignals: dedup (Q10) ---

func TestIngestSignals_DuplicateIsNoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{
		ID:         "evt-1",
		OccurredAt: "2026-08-20T01:00:00Z",
		Identity:   "jira:ROOKPF-1",
		Title:      "first",
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "jira-api", "jira-cloud/jira-cloud", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Re-send the SAME id, even with different content — PK-based dedup
	// (INSERT OR IGNORE) must treat this as a no-op regardless of payload
	// drift, not just exact-duplicate payloads.
	row2 := row
	row2.Title = "second (should be ignored)"
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "jira-api", "jira-cloud/jira-cloud", []orchestrator.SignalIngestRow{row2}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1 (duplicate must be a no-op): %+v", len(signals), signals)
	}
	if signals[0].Title != "first" {
		t.Errorf("Title = %q, want %q (INSERT OR IGNORE must not overwrite)", signals[0].Title, "first")
	}
}

// --- IngestSignals: cursor advance (offset-mixed, time not string compare) ---

func TestIngestSignals_CursorAdvancesUsingParsedTimeNotStringCompare(t *testing.T) {
	d := testutil.NewTestDB(t)

	// occurred_at = 01:00 UTC.
	row1 := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row1}); err != nil {
		t.Fatalf("ingest row1: %v", err)
	}
	cur, err := orchestrator.GetSignalCursor(d.Conn, "ws-1", "svc", "pack/conn")
	if err != nil {
		t.Fatalf("GetSignalCursor: %v", err)
	}
	if !mustParseRFC3339(t, cur).Equal(mustParseRFC3339(t, "2026-08-20T01:00:00Z")) {
		t.Fatalf("cursor after row1 = %q, want 2026-08-20T01:00:00Z (equal instant)", cur)
	}

	// -09:00 offset: "2026-08-19T23:00:00-09:00" == 2026-08-20T08:00:00Z —
	// LATER in real time than the current cursor (01:00Z), but its raw
	// string sorts BEFORE the cursor's string ("2026-08-19" < "2026-08-20"
	// lexicographically). A string-compare implementation would wrongly
	// treat this as "not an advance"; the correct implementation parses
	// both as time.Time and compares instants.
	row2 := orchestrator.SignalIngestRow{ID: "evt-2", OccurredAt: "2026-08-19T23:00:00-09:00", Identity: "jira:B"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row2}); err != nil {
		t.Fatalf("ingest row2: %v", err)
	}
	cur, err = orchestrator.GetSignalCursor(d.Conn, "ws-1", "svc", "pack/conn")
	if err != nil {
		t.Fatalf("GetSignalCursor: %v", err)
	}
	wantInstant := mustParseRFC3339(t, "2026-08-20T08:00:00Z")
	if !mustParseRFC3339(t, cur).Equal(wantInstant) {
		t.Fatalf("cursor after row2 = %q, want an instant equal to 2026-08-20T08:00:00Z (parsed-time compare, not string compare)", cur)
	}
}

func TestIngestSignals_CursorNeverRegresses(t *testing.T) {
	d := testutil.NewTestDB(t)

	advanced := orchestrator.SignalIngestRow{ID: "evt-late", OccurredAt: "2026-08-20T08:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{advanced}); err != nil {
		t.Fatalf("ingest advanced: %v", err)
	}
	older := orchestrator.SignalIngestRow{ID: "evt-early", OccurredAt: "2026-08-20T00:00:00Z", Identity: "jira:B"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{older}); err != nil {
		t.Fatalf("ingest older: %v", err)
	}

	cur, err := orchestrator.GetSignalCursor(d.Conn, "ws-1", "svc", "pack/conn")
	if err != nil {
		t.Fatalf("GetSignalCursor: %v", err)
	}
	want := mustParseRFC3339(t, "2026-08-20T08:00:00Z")
	if !mustParseRFC3339(t, cur).Equal(want) {
		t.Fatalf("cursor regressed: got %q, want it to stay at 2026-08-20T08:00:00Z", cur)
	}
}

func TestGetSignalCursor_EmptyWhenNoRows(t *testing.T) {
	d := testutil.NewTestDB(t)
	cur, err := orchestrator.GetSignalCursor(d.Conn, "ws-1", "svc", "pack/conn")
	if err != nil {
		t.Fatalf("GetSignalCursor: %v", err)
	}
	if cur != "" {
		t.Errorf("cursor = %q, want empty string for a source that has never ingested", cur)
	}
}

// --- IngestSignals: transaction atomicity (Q13) ---

// TestIngestSignals_RollbackLeavesNoPartialState pins Q13 at the store
// function's own boundary: IngestSignals's INSERT loop and its cursor
// advance MUST be part of the SAME transaction the caller controls — if
// the surrounding transaction is rolled back (simulating a daemon crash
// before commit), NEITHER the signal rows NOR the cursor advance may
// survive. A partial commit (rows inserted but cursor not advanced, or vice
// versa) would violate signal-driven-review.md §8.1's "no stuck
// mid-ingestion terminal state" invariant.
func TestIngestSignals_RollbackLeavesNoPartialState(t *testing.T) {
	d := testutil.NewTestDB(t)

	rows := []orchestrator.SignalIngestRow{
		{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"},
		{ID: "evt-2", OccurredAt: "2026-08-20T02:00:00Z", Identity: "jira:B"},
	}

	sentinelErr := errors.New("simulated crash before commit")
	err := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		if err := orchestrator.IngestSignals(tx, "ws-1", "svc", "pack/conn", rows); err != nil {
			t.Fatalf("IngestSignals inside tx: %v", err)
		}
		// Force the transaction to roll back — the moral equivalent of the
		// daemon process dying after the INSERTs/UPDATE ran in-memory but
		// before COMMIT reached disk.
		return sentinelErr
	})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("InTxDB error = %v, want sentinelErr to propagate", err)
	}

	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("got %d signals after rollback, want 0 (rows must not survive a rolled-back tx)", len(signals))
	}
	cur, err := orchestrator.GetSignalCursor(d.Conn, "ws-1", "svc", "pack/conn")
	if err != nil {
		t.Fatalf("GetSignalCursor: %v", err)
	}
	if cur != "" {
		t.Fatalf("cursor = %q after rollback, want empty (cursor advance must not survive a rolled-back tx either)", cur)
	}

	// Retry (what the connector does on its next tick after a crash): the
	// same rows must be ingestible from scratch, proving no permanent loss.
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", rows); err != nil {
		t.Fatalf("retry ingest: %v", err)
	}
	signals, err = orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals after retry: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("got %d signals after retry, want 2", len(signals))
	}
	cur, err = orchestrator.GetSignalCursor(d.Conn, "ws-1", "svc", "pack/conn")
	if err != nil {
		t.Fatalf("GetSignalCursor after retry: %v", err)
	}
	if cur == "" {
		t.Fatal("cursor still empty after successful retry")
	}
}

// TestTaskRepository_IngestSignals_PartialBatchFailureRollsBackEntirely pins
// Q13 at the TaskRepository wrapper boundary (the shape most future callers
// will actually use, constructed with a raw *sql.DB — nothing in THIS PR
// wires it inside an existing WithinTx yet). A batch with a bad row midway
// through must not leave the earlier, individually-valid rows behind: the
// wrapper must open its OWN transaction spanning the whole call (same
// pattern as TaskRepository.DeleteTask), not just delegate to the package
// function against the raw *sql.DB (whose statements would each
// autocommit independently).
func TestTaskRepository_IngestSignals_PartialBatchFailureRollsBackEntirely(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := orchestrator.NewTaskRepository(d.Conn)

	rows := []orchestrator.SignalIngestRow{
		{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"},
		{ID: "evt-2", OccurredAt: "not-a-valid-timestamp", Identity: "jira:B"}, // invalid: mid-batch failure
	}

	if err := repo.IngestSignals("ws-1", "svc", "pack/conn", rows); err == nil {
		t.Fatal("IngestSignals with an invalid row = nil error, want an error")
	}

	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("got %d signals after a failed batch, want 0 (evt-1 must have rolled back with evt-2's failure)", len(signals))
	}
}

// --- IngestSignals: validation ---

func TestIngestSignals_RowOverSizeLimitRejected(t *testing.T) {
	d := testutil.NewTestDB(t)
	huge := orchestrator.SignalIngestRow{
		ID:         "evt-1",
		OccurredAt: "2026-08-20T01:00:00Z",
		Identity:   "jira:A",
		Title:      strings.Repeat("a", orchestrator.MaxContentBytes+1),
	}
	err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{huge})
	if err == nil {
		t.Fatal("IngestSignals with an oversized row = nil error, want an error")
	}
	var sizeErr *orchestrator.ContentSizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("error = %v, want a *ContentSizeError", err)
	}

	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("got %d signals, want 0 (oversized row must not be inserted)", len(signals))
	}
}

func TestIngestSignals_MissingRequiredFieldsRejected(t *testing.T) {
	base := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}

	tests := []struct {
		name string
		row  orchestrator.SignalIngestRow
	}{
		{"empty id", func() orchestrator.SignalIngestRow { r := base; r.ID = ""; return r }()},
		{"empty identity", func() orchestrator.SignalIngestRow { r := base; r.Identity = ""; return r }()},
		{"unparsable occurred_at", func() orchestrator.SignalIngestRow { r := base; r.OccurredAt = "not-a-timestamp"; return r }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testutil.NewTestDB(t)
			if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{tt.row}); err == nil {
				t.Fatal("IngestSignals = nil error, want an error")
			}
		})
	}
}

func TestIngestSignals_EmptyScopeRejected(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err == nil {
		t.Fatal("IngestSignals with empty workspace id = nil error, want an error")
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "", "pack/conn", []orchestrator.SignalIngestRow{row}); err == nil {
		t.Fatal("IngestSignals with empty service = nil error, want an error")
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "", []orchestrator.SignalIngestRow{row}); err == nil {
		t.Fatal("IngestSignals with empty connector = nil error, want an error")
	}
}

// --- AckSignals ---

func TestAckSignals_Idempotent(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-1"}); err != nil {
		t.Fatalf("first ack: %v", err)
	}
	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAcked})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 1 || signals[0].AckedAt == nil {
		t.Fatalf("signal not acked after first AckSignals: %+v", signals)
	}
	firstAckedAt := *signals[0].AckedAt

	// Second ack of the same, already-acked id must be a no-op success, not
	// an error (signal-driven-review.md §8.1: "ack は冪等").
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-1"}); err != nil {
		t.Fatalf("second (idempotent) ack: %v", err)
	}
	signals, err = orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAcked})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 1 || signals[0].AckedAt == nil {
		t.Fatalf("signal not found acked after second AckSignals: %+v", signals)
	}
	if !signals[0].AckedAt.Equal(firstAckedAt) {
		t.Errorf("acked_at changed on idempotent re-ack: %v -> %v", firstAckedAt, *signals[0].AckedAt)
	}
}

func TestAckSignals_UnknownIDErrors(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-1", "evt-typo"})
	if err == nil {
		t.Fatal("AckSignals with an unknown id = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "evt-typo") {
		t.Errorf("error %q should name the unknown id", err.Error())
	}

	// The known id in the same call must NOT have been acked — an error
	// return means the whole call failed, not "best effort".
	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 1 || signals[0].AckedAt != nil {
		t.Fatalf("evt-1 was acked despite the call erroring on evt-typo: %+v", signals)
	}
}

func TestAckSignals_MatchesAcrossServiceAndConnectorByIDAlone(t *testing.T) {
	d := testutil.NewTestDB(t)
	rowA := orchestrator.SignalIngestRow{ID: "evt-shared", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	rowB := orchestrator.SignalIngestRow{ID: "evt-shared", OccurredAt: "2026-08-20T01:05:00Z", Identity: "slack:B"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc-a", "pack/connA", []orchestrator.SignalIngestRow{rowA}); err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc-b", "pack/connB", []orchestrator.SignalIngestRow{rowB}); err != nil {
		t.Fatalf("ingest B: %v", err)
	}

	// §2 store contract: AckSignals matches on (workspace_id, id) ONLY —
	// every row sharing that id across service/connector is acked.
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-shared"}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("got %d signals, want 2", len(signals))
	}
	for _, s := range signals {
		if s.AckedAt == nil {
			t.Errorf("signal (service=%s connector=%s) not acked", s.Service, s.Connector)
		}
	}
}

func TestAckSignals_ScopedByWorkspace(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-other", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// evt-1 exists, but in a DIFFERENT workspace — acking it from ws-1 must
	// report it as unknown, not silently cross workspace scope.
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-1"}); err == nil {
		t.Fatal("AckSignals for an id that only exists in another workspace = nil error, want an error")
	}

	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-other", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 1 || signals[0].AckedAt != nil {
		t.Fatalf("ws-other's signal must remain unacked: %+v", signals)
	}
}

// --- ClaimSignals ---

func TestClaimSignals_IncrementsAttemptsAndExcludesDead(t *testing.T) {
	d := testutil.NewTestDB(t)
	fresh := orchestrator.SignalIngestRow{ID: "evt-fresh", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{fresh}); err != nil {
		t.Fatalf("ingest fresh: %v", err)
	}
	dead := orchestrator.SignalIngestRow{ID: "evt-dead", OccurredAt: "2026-08-20T00:00:00Z", Identity: "jira:B"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{dead}); err != nil {
		t.Fatalf("ingest dead: %v", err)
	}
	// Drive evt-dead's attempts up to the max via repeated claims so it
	// becomes dead (attempts >= maxAttempts).
	for i := 0; i < 5; i++ {
		if _, err := orchestrator.ClaimSignals(d.Conn, "ws-1", 1, 5); err != nil {
			t.Fatalf("claim #%d: %v", i, err)
		}
	}
	// After 5 claims, evt-dead (occurred first, so claimed first each time)
	// has attempts=5 and is dead; evt-fresh is untouched (attempts=0).

	claimed, err := orchestrator.ClaimSignals(d.Conn, "ws-1", 10, 5)
	if err != nil {
		t.Fatalf("claim remaining: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("got %d claimed signals, want 1 (dead evt-dead must be excluded): %+v", len(claimed), claimed)
	}
	if claimed[0].ID != "evt-fresh" {
		t.Errorf("claimed id = %q, want evt-fresh", claimed[0].ID)
	}
	if claimed[0].Attempts != 1 {
		t.Errorf("claimed attempts = %d, want 1", claimed[0].Attempts)
	}

	// evt-dead itself must still be visible as dead, not silently dropped.
	deadList, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateDead, MaxAttempts: 5})
	if err != nil {
		t.Fatalf("ListSignals(dead): %v", err)
	}
	if len(deadList) != 1 || deadList[0].ID != "evt-dead" {
		t.Fatalf("dead list = %+v, want exactly evt-dead", deadList)
	}
}

func TestClaimSignals_OrdersByOccurredAtAscendingAndRespectsLimit(t *testing.T) {
	d := testutil.NewTestDB(t)
	rows := []orchestrator.SignalIngestRow{
		{ID: "evt-3", OccurredAt: "2026-08-20T03:00:00Z", Identity: "jira:C"},
		{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"},
		{ID: "evt-2", OccurredAt: "2026-08-20T02:00:00Z", Identity: "jira:B"},
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	claimed, err := orchestrator.ClaimSignals(d.Conn, "ws-1", 2, 5)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("got %d claimed, want 2 (limit)", len(claimed))
	}
	if claimed[0].ID != "evt-1" || claimed[1].ID != "evt-2" {
		t.Fatalf("claim order = [%s, %s], want [evt-1, evt-2] (occurred_at ascending)", claimed[0].ID, claimed[1].ID)
	}
}

// --- HasPendingSignals ---

func TestHasPendingSignals_TrueWhenPendingExists(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	has, err := orchestrator.HasPendingSignals(d.Conn, "ws-1", 5)
	if err != nil {
		t.Fatalf("HasPendingSignals: %v", err)
	}
	if !has {
		t.Error("HasPendingSignals = false, want true")
	}
}

func TestHasPendingSignals_FalseWhenOnlyDead(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Drive attempts to maxAttempts (2, for a fast test) so the only
	// signal in the workspace is dead.
	for i := 0; i < 2; i++ {
		if _, err := orchestrator.ClaimSignals(d.Conn, "ws-1", 10, 2); err != nil {
			t.Fatalf("claim #%d: %v", i, err)
		}
	}

	has, err := orchestrator.HasPendingSignals(d.Conn, "ws-1", 2)
	if err != nil {
		t.Fatalf("HasPendingSignals: %v", err)
	}
	// signal-driven-review.md §8.1 / design doc §2: dead must not count as
	// pending, or a workspace with only dead-lettered signals would fire
	// its trigger forever.
	if has {
		t.Error("HasPendingSignals = true with only dead signals, want false")
	}
}

func TestHasPendingSignals_FalseWhenOnlyAcked(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-1"}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	has, err := orchestrator.HasPendingSignals(d.Conn, "ws-1", 5)
	if err != nil {
		t.Fatalf("HasPendingSignals: %v", err)
	}
	if has {
		t.Error("HasPendingSignals = true with only acked signals, want false")
	}
}

// --- ListSignals: state filters ---

func TestListSignals_StateFilters(t *testing.T) {
	d := testutil.NewTestDB(t)
	pending := orchestrator.SignalIngestRow{ID: "evt-pending", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	deadRow := orchestrator.SignalIngestRow{ID: "evt-dead", OccurredAt: "2026-08-20T00:00:00Z", Identity: "jira:B"}
	acked := orchestrator.SignalIngestRow{ID: "evt-acked", OccurredAt: "2026-08-20T02:00:00Z", Identity: "jira:C"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{pending, deadRow, acked}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-acked"}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// evt-dead occurs before evt-pending, so it's claimed first each round.
	for i := 0; i < 2; i++ {
		if _, err := orchestrator.ClaimSignals(d.Conn, "ws-1", 1, 2); err != nil {
			t.Fatalf("claim #%d: %v", i, err)
		}
	}

	cases := []struct {
		state orchestrator.SignalState
		want  []string
	}{
		{orchestrator.SignalStatePending, []string{"evt-pending"}},
		{orchestrator.SignalStateDead, []string{"evt-dead"}},
		{orchestrator.SignalStateAcked, []string{"evt-acked"}},
		{orchestrator.SignalStateAll, []string{"evt-dead", "evt-pending", "evt-acked"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			got, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: tc.state, MaxAttempts: 2})
			if err != nil {
				t.Fatalf("ListSignals(%s): %v", tc.state, err)
			}
			gotIDs := make([]string, len(got))
			for i, s := range got {
				gotIDs[i] = s.ID
			}
			if len(gotIDs) != len(tc.want) {
				t.Fatalf("ListSignals(%s) ids = %v, want (same set as) %v", tc.state, gotIDs, tc.want)
			}
			wantSet := map[string]bool{}
			for _, id := range tc.want {
				wantSet[id] = true
			}
			for _, id := range gotIDs {
				if !wantSet[id] {
					t.Errorf("ListSignals(%s) unexpectedly includes %q", tc.state, id)
				}
			}
		})
	}
}

func TestListSignals_RequiresWorkspaceID(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{})
	if err == nil {
		t.Fatal("ListSignals with empty WorkspaceID = nil error, want an error")
	}
}

func TestListSignals_ScopedByServiceAndConnector(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc-a", "pack/connA", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc-b", "pack/connB", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest B: %v", err)
	}

	got, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", Service: "svc-a", Connector: "pack/connA", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(got) != 1 || got[0].Service != "svc-a" {
		t.Fatalf("got %+v, want exactly the svc-a row", got)
	}
}

// --- GCSignals ---

func TestGCSignals_DeletesOldAckedRows(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-1"}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Backdate acked_at directly so it's older than the GC cutoff.
	if _, err := d.Conn.Exec(`UPDATE signals SET acked_at = ? WHERE id = 'evt-1'`, "2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("backdate acked_at: %v", err)
	}

	n, err := orchestrator.GCSignals(d.Conn, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("GCSignals: %v", err)
	}
	if n != 1 {
		t.Fatalf("GCSignals deleted %d rows, want 1", n)
	}
	remaining, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("got %d remaining signals, want 0", len(remaining))
	}
}

// TestGCSignals_DeletesOldUnackedRows pins §2's "未 ack でも received_at が
// cutoff より古い行を削除" — the dead-letter permanent-residency prevention
// (design doc §2 table, GCSignals row).
func TestGCSignals_DeletesOldUnackedRows(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE signals SET received_at = ? WHERE id = 'evt-1'`, "2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("backdate received_at: %v", err)
	}

	n, err := orchestrator.GCSignals(d.Conn, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("GCSignals: %v", err)
	}
	if n != 1 {
		t.Fatalf("GCSignals deleted %d rows, want 1 (old unacked/dead row must be reaped)", n)
	}
}

func TestGCSignals_KeepsFreshRowsRegardlessOfAckState(t *testing.T) {
	d := testutil.NewTestDB(t)
	rows := []orchestrator.SignalIngestRow{
		{ID: "evt-pending", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"},
		{ID: "evt-acked", OccurredAt: "2026-08-20T02:00:00Z", Identity: "jira:B"},
	}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := orchestrator.AckSignals(d.Conn, "ws-1", []string{"evt-acked"}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	n, err := orchestrator.GCSignals(d.Conn, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("GCSignals: %v", err)
	}
	if n != 0 {
		t.Fatalf("GCSignals deleted %d fresh rows, want 0", n)
	}
}

func TestGCSignals_DryRunDoesNotDelete(t *testing.T) {
	d := testutil.NewTestDB(t)
	row := orchestrator.SignalIngestRow{ID: "evt-1", OccurredAt: "2026-08-20T01:00:00Z", Identity: "jira:A"}
	if err := orchestrator.IngestSignals(d.Conn, "ws-1", "svc", "pack/conn", []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE signals SET received_at = ? WHERE id = 'evt-1'`, "2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("backdate received_at: %v", err)
	}

	n, err := orchestrator.GCSignals(d.Conn, 24*time.Hour, true)
	if err != nil {
		t.Fatalf("GCSignals dry-run: %v", err)
	}
	if n != 1 {
		t.Fatalf("GCSignals dry-run count = %d, want 1", n)
	}
	remaining, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("dry-run must not delete: got %d remaining, want 1", len(remaining))
	}
}

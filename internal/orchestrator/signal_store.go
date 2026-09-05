package orchestrator

// Store layer for the signal inbox (migration 0046). Two invariants:
// dedup is the PRIMARY KEY itself (workspace_id, service, connector, id);
// each source's cursor advances monotonically and never moves backward.
// occurred_at/cursor comparisons always go through time.Time (parsed RFC3339),
// never raw string comparison, since a tz-offset-bearing timestamp string
// does not sort chronologically as text. See docs/plans/
// signal-ingest-detailed-design.md and signal-driven-review.md §8.1.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// MaxSignalAttempts is the retry ceiling before a signal is considered dead.
// Store functions take maxAttempts as an explicit parameter rather than
// baking this in directly, so tests can exercise the dead-letter boundary
// without waiting through 5 real claims.
const MaxSignalAttempts = 5

// DefaultSignalListLimit / MaxSignalListLimit bound ListSignals/ClaimSignals
// row counts, mirroring action_list.go's Default/MaxActionListLimit.
const (
	DefaultSignalListLimit = 200
	MaxSignalListLimit     = 1000
)

// ClampSignalListLimit resolves a caller-requested limit (<=0 meaning "use
// the default") the same way action_list.go's ClampActionListLimit does.
func ClampSignalListLimit(requested int) int {
	if requested <= 0 {
		return DefaultSignalListLimit
	}
	if requested > MaxSignalListLimit {
		return MaxSignalListLimit
	}
	return requested
}

// SignalState selects a subset of a workspace's inbox for ListSignals.
type SignalState string

const (
	// SignalStatePending: acked_at IS NULL AND attempts < maxAttempts.
	SignalStatePending SignalState = "pending"
	// SignalStateDead: acked_at IS NULL AND attempts >= maxAttempts — the
	// row is not deleted, just excluded from ClaimSignals/HasPendingSignals
	// until a human acks it or GC reaps it.
	SignalStateDead SignalState = "dead"
	// SignalStateAcked: acked_at IS NOT NULL.
	SignalStateAcked SignalState = "acked"
	// SignalStateAll: no state filter.
	SignalStateAll SignalState = "all"
)

// Signal is one row of the signals table.
type Signal struct {
	WorkspaceID string
	Service     string
	Connector   string
	ID          string
	OccurredAt  time.Time
	Identity    string
	URL         string
	Author      string
	Title       string
	ReceivedAt  time.Time
	Attempts    int
	// AckedAt is nil while the signal is unacked.
	AckedAt *time.Time
}

// SignalIngestRow is one Signal as read from a connector's JSONL output: one
// line = `{id, occurred_at, identity, url?, author?, title?}`.
// ID/OccurredAt/Identity are required; URL/Author/Title default to "".
type SignalIngestRow struct {
	ID         string `json:"id"`
	OccurredAt string `json:"occurred_at"` // RFC3339, tz-aware
	Identity   string `json:"identity"`
	URL        string `json:"url,omitempty"`
	Author     string `json:"author,omitempty"`
	Title      string `json:"title,omitempty"`
}

// SignalFilter scopes and paginates ListSignals. WorkspaceID is required;
// there is no "give me everything" fallback.
type SignalFilter struct {
	WorkspaceID string
	Service     string
	Connector   string
	// State defaults to SignalStatePending when empty.
	State SignalState
	Limit int
	// MaxAttempts, when <= 0, defaults to MaxSignalAttempts.
	MaxAttempts int
}

func resolveMaxAttempts(maxAttempts int) int {
	if maxAttempts <= 0 {
		return MaxSignalAttempts
	}
	return maxAttempts
}

// signalTimeLayout is a FIXED-WIDTH RFC3339-with-nanoseconds layout used for
// every stored timestamp. Deliberately not time.RFC3339Nano: that trims
// trailing fractional-second zeros, producing a variable-width string whose
// lexicographic order (signals is ORDER BY'd as plain TEXT) does not always
// match chronological order.
const signalTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatSignalTime(t time.Time) string {
	return t.UTC().Format(signalTimeLayout)
}

// parseSignalTime parses an RFC3339 timestamp. Go's time.Parse tolerates a
// fractional-second component even though time.RFC3339's layout string
// doesn't spell one out, so this also accepts what formatSignalTime writes
// (RFC3339Nano) without needing two layouts.
func parseSignalTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// IngestSignals is a connector's single entry point into the inbox: for
// each row it INSERT OR IGNOREs into signals (PK-based dedup) and then
// advances (workspace, service, connector)'s cursor to max(rows.OccurredAt),
// but only forward, never backward.
//
// Callers MUST invoke this within a single transaction (dbtx must be a
// db.DBTX backed by an open *sql.Tx, or the whole call must itself be the
// only statement running against a *sql.DB) — the INSERTs and the cursor
// UPDATE/INSERT are only atomic as a unit if dbtx guarantees it. See
// TaskRepository.IngestSignals (repository.go) for the wrapper that
// provides this guarantee when given a raw *sql.DB.
func IngestSignals(dbtx db.DBTX, workspaceID, service, connector string, rows []SignalIngestRow) error {
	if workspaceID == "" {
		return fmt.Errorf("ingest signals: workspace id must not be empty")
	}
	// service MAY be empty: boid's own internal-signal source never reaches
	// an external service, so its envelope's source.service is "".
	if connector == "" {
		return fmt.Errorf("ingest signals: connector must not be empty")
	}
	if len(rows) == 0 {
		return nil
	}

	receivedAt := formatSignalTime(time.Now())
	var maxOccurred time.Time
	haveMax := false

	for _, row := range rows {
		if row.ID == "" {
			return fmt.Errorf("ingest signals: row id must not be empty")
		}
		if row.Identity == "" {
			return fmt.Errorf("ingest signals: row %q: identity must not be empty", row.ID)
		}
		occurredAt, err := parseSignalTime(row.OccurredAt)
		if err != nil {
			return fmt.Errorf("ingest signals: row %q: parse occurred_at %q: %w", row.ID, row.OccurredAt, err)
		}

		encoded, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("ingest signals: row %q: encode: %w", row.ID, err)
		}
		if err := ValidateContentSize("signal", encoded); err != nil {
			return fmt.Errorf("ingest signals: row %q: %w", row.ID, err)
		}

		if _, err := dbtx.Exec(
			`INSERT OR IGNORE INTO signals
			 (workspace_id, service, connector, id, occurred_at, identity, url, author, title, received_at, attempts, acked_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)`,
			workspaceID, service, connector, row.ID, formatSignalTime(occurredAt), row.Identity, row.URL, row.Author, row.Title, receivedAt,
		); err != nil {
			return fmt.Errorf("ingest signals: row %q: insert: %w", row.ID, err)
		}

		if !haveMax || occurredAt.After(maxOccurred) {
			maxOccurred = occurredAt
			haveMax = true
		}
	}

	if !haveMax {
		return nil
	}

	current, err := GetSignalCursor(dbtx, workspaceID, service, connector)
	if err != nil {
		return fmt.Errorf("ingest signals: read cursor: %w", err)
	}
	advance := true
	if current != "" {
		currentTime, err := parseSignalTime(current)
		if err != nil {
			return fmt.Errorf("ingest signals: parse existing cursor %q: %w", current, err)
		}
		// Monotonic-only: a batch entirely at or before the current cursor
		// must not move it backward.
		if !maxOccurred.After(currentTime) {
			advance = false
		}
	}
	if advance {
		if err := setSignalCursor(dbtx, workspaceID, service, connector, formatSignalTime(maxOccurred)); err != nil {
			return err
		}
	}
	return nil
}

// GetSignalCursor returns (workspaceID, service, connector)'s stored cursor,
// or "" if the source has never ingested.
//
// Contract: the cursor is exclusive of itself from the connector's point of
// view — a conformant connector drops anything with `occurred_at <= cursor`
// before calling `boid signal ingest`. This store just returns the opaque
// stored value; PK-based dedup in IngestSignals is a separate, independent
// safety net for exact-duplicate re-sends.
//
// Known risk: if two sibling events share the exact same occurred_at and the
// cursor advances past that timestamp after only one is ingested, the
// connector's own `<= cursor` self-filter can silently drop the sibling
// forever. Pack authors working with tie-prone sources should add a
// secondary tiebreaker (e.g. id) in their own fetch logic.
func GetSignalCursor(dbtx db.DBTX, workspaceID, service, connector string) (string, error) {
	var cursor string
	err := dbtx.QueryRow(
		`SELECT cursor FROM signal_cursors WHERE workspace_id = ? AND service = ? AND connector = ?`,
		workspaceID, service, connector,
	).Scan(&cursor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get signal cursor: %w", err)
	}
	return cursor, nil
}

func setSignalCursor(dbtx db.DBTX, workspaceID, service, connector, cursor string) error {
	_, err := dbtx.Exec(
		`INSERT INTO signal_cursors (workspace_id, service, connector, cursor, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (workspace_id, service, connector)
		 DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		workspaceID, service, connector, cursor, formatSignalTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("set signal cursor: %w", err)
	}
	return nil
}

// signalStateCondition returns the SQL condition and any extra args (beyond
// maxAttempts, which callers append themselves when the placeholder is
// present) for a given SignalState. Shared by ListSignals/HasPendingSignals
// so the pending/dead predicate is defined in exactly one place.
func signalStateCondition(state SignalState) (cond string, usesMaxAttempts bool, err error) {
	switch state {
	case "", SignalStatePending:
		return "acked_at IS NULL AND attempts < ?", true, nil
	case SignalStateDead:
		return "acked_at IS NULL AND attempts >= ?", true, nil
	case SignalStateAcked:
		return "acked_at IS NOT NULL", false, nil
	case SignalStateAll:
		return "1 = 1", false, nil
	default:
		return "", false, fmt.Errorf("list signals: unknown state %q", state)
	}
}

// ListSignals returns filter.WorkspaceID's signals (optionally narrowed by
// Service/Connector/State), ordered by occurred_at ascending (oldest first —
// same processing order ClaimSignals uses).
func ListSignals(dbtx db.DBTX, filter SignalFilter) ([]*Signal, error) {
	if filter.WorkspaceID == "" {
		return nil, fmt.Errorf("list signals: workspace id must not be empty")
	}
	stateCond, usesMaxAttempts, err := signalStateCondition(filter.State)
	if err != nil {
		return nil, err
	}

	conds := []string{"workspace_id = ?", stateCond}
	args := []any{filter.WorkspaceID}
	if usesMaxAttempts {
		args = append(args, resolveMaxAttempts(filter.MaxAttempts))
	}
	if filter.Service != "" {
		conds = append(conds, "service = ?")
		args = append(args, filter.Service)
	}
	if filter.Connector != "" {
		conds = append(conds, "connector = ?")
		args = append(args, filter.Connector)
	}
	args = append(args, ClampSignalListLimit(filter.Limit))

	query := `SELECT ` + signalSelectCols + ` FROM signals WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY occurred_at ASC, id ASC LIMIT ?`
	rows, err := dbtx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list signals: %w", err)
	}
	defer rows.Close()
	return scanSignals(rows)
}

// ClaimSignals selects up to limit pending signals (occurred_at ascending)
// for workspaceID, increments each one's attempts, and returns the
// post-increment rows. Dead signals (attempts >= maxAttempts) are never
// selected.
//
// DEPRECATED: prefer ListSignals + ClaimSignalIDs — charging on the read
// conflates "I looked at this" with "I handed this to a judgment"; see
// ClaimSignalIDs' doc comment. Kept until no workspace calls it.
//
// Like IngestSignals, this does a select then a write that must be one
// atomic unit — see TaskRepository.ClaimSignals for the wrapper that
// provides that guarantee automatically.
func ClaimSignals(dbtx db.DBTX, workspaceID string, limit, maxAttempts int) ([]*Signal, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("claim signals: workspace id must not be empty")
	}
	maxAttempts = resolveMaxAttempts(maxAttempts)
	limit = ClampSignalListLimit(limit)

	rows, err := dbtx.Query(
		`SELECT service, connector, id FROM signals
		 WHERE workspace_id = ? AND acked_at IS NULL AND attempts < ?
		 ORDER BY occurred_at ASC, id ASC LIMIT ?`,
		workspaceID, maxAttempts, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim signals: select: %w", err)
	}
	type key struct{ service, connector, id string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.service, &k.connector, &k.id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim signals: scan: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("claim signals: rows: %w", err)
	}
	rows.Close()

	claimed := make([]*Signal, 0, len(keys))
	for _, k := range keys {
		if _, err := dbtx.Exec(
			`UPDATE signals SET attempts = attempts + 1
			 WHERE workspace_id = ? AND service = ? AND connector = ? AND id = ?`,
			workspaceID, k.service, k.connector, k.id,
		); err != nil {
			return nil, fmt.Errorf("claim signals: update: %w", err)
		}
		row := dbtx.QueryRow(
			`SELECT `+signalSelectCols+` FROM signals
			 WHERE workspace_id = ? AND service = ? AND connector = ? AND id = ?`,
			workspaceID, k.service, k.connector, k.id,
		)
		sig, err := scanSignalRow(row)
		if err != nil {
			return nil, fmt.Errorf("claim signals: re-fetch: %w", err)
		}
		claimed = append(claimed, sig)
	}
	return claimed, nil
}

// ClaimSignalIDs charges one attempt against exactly the ids named — the
// explicit half of the read/claim split: `boid signal list` reads with no
// side effect, `boid signal claim <id>...` says which rows were actually
// handed to a judgment (unlike ClaimSignals, which charges every row a
// consumer's read merely returned, even ones it never judged).
//
// Same typo guard and all-or-nothing failure as AckSignals. An already-acked
// row is not charged but is not an error either.
func ClaimSignalIDs(dbtx db.DBTX, workspaceID string, ids []string) error {
	if workspaceID == "" {
		return fmt.Errorf("claim signals: workspace id must not be empty")
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders, args, err := requireSignalIDsExist(dbtx, "claim signals", workspaceID, ids)
	if err != nil {
		return err
	}

	if _, err := dbtx.Exec(
		`UPDATE signals SET attempts = attempts + 1
		 WHERE workspace_id = ? AND acked_at IS NULL AND id IN (`+placeholders+`)`,
		args...,
	); err != nil {
		return fmt.Errorf("claim signals: update: %w", err)
	}
	return nil
}

// requireSignalIDsExist is the shared pre-flight both AckSignals and
// ClaimSignalIDs run: dedupe the ids, confirm every one of them matches a
// row in workspaceID, and hand back the placeholder list and args (workspace
// id first, then the unique ids) for the caller's own UPDATE. This existence
// check is a typo guard: it runs before the write and fails the whole call,
// so a mistyped id never leaves the others partially applied.
func requireSignalIDsExist(dbtx db.DBTX, op, workspaceID string, ids []string) (placeholders string, args []any, err error) {
	unique := dedupeStringsPreserveOrder(ids)
	placeholders = strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")

	args = make([]any, 0, len(unique)+1)
	args = append(args, workspaceID)
	for _, id := range unique {
		args = append(args, id)
	}

	existing := map[string]bool{}
	rows, qerr := dbtx.Query(`SELECT DISTINCT id FROM signals WHERE workspace_id = ? AND id IN (`+placeholders+`)`, args...)
	if qerr != nil {
		return "", nil, fmt.Errorf("%s: lookup: %w", op, qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			return "", nil, fmt.Errorf("%s: scan: %w", op, serr)
		}
		existing[id] = true
	}
	if rerr := rows.Err(); rerr != nil {
		return "", nil, fmt.Errorf("%s: rows: %w", op, rerr)
	}

	var unknown []string
	for _, id := range unique {
		if !existing[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", nil, fmt.Errorf("%s: unknown id(s) in workspace %q: %s", op, workspaceID, strings.Join(unknown, ", "))
	}
	return placeholders, args, nil
}

// AckSignals marks each id acked within workspaceID (acked_at = now WHERE
// acked_at IS NULL — already-acked rows are left untouched, making re-ack a
// no-op success rather than an error). Matching is by (workspace_id, id)
// ONLY (§2: id is not workspace-unique across service/connector) — every
// row sharing that id, regardless of service/connector, is acked.
//
// Returns an error naming every id that has no matching signal row in
// workspaceID (typo detection, §2) — the call fails entirely in that case;
// none of the given ids (known or unknown) are acked, so a caller can retry
// after fixing the typo without wondering what partially applied.
func AckSignals(dbtx db.DBTX, workspaceID string, ids []string) error {
	if workspaceID == "" {
		return fmt.Errorf("ack signals: workspace id must not be empty")
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders, args, err := requireSignalIDsExist(dbtx, "ack signals", workspaceID, ids)
	if err != nil {
		return err
	}

	updateArgs := append([]any{formatSignalTime(time.Now())}, args...)
	if _, err := dbtx.Exec(
		`UPDATE signals SET acked_at = ? WHERE workspace_id = ? AND acked_at IS NULL AND id IN (`+placeholders+`)`,
		updateArgs...,
	); err != nil {
		return fmt.Errorf("ack signals: update: %w", err)
	}
	return nil
}

// HasPendingSignals reports whether workspaceID has at least one pending
// (unacked, attempts < maxAttempts) signal — the `on: signals` trigger
// predicate. Dead signals never count, so a workspace with only
// dead-lettered signals does not fire its trigger forever.
func HasPendingSignals(dbtx db.DBTX, workspaceID string, maxAttempts int) (bool, error) {
	if workspaceID == "" {
		return false, fmt.Errorf("has pending signals: workspace id must not be empty")
	}
	maxAttempts = resolveMaxAttempts(maxAttempts)
	var exists int
	err := dbtx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM signals WHERE workspace_id = ? AND acked_at IS NULL AND attempts < ? LIMIT 1)`,
		workspaceID, maxAttempts,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("has pending signals: %w", err)
	}
	return exists != 0, nil
}

// GCSignals deletes signals rows that are structurally eligible — acked or
// dead-lettered (attempts >= MaxSignalAttempts) — and, when olderThan > 0,
// additionally old enough (acked_at, respectively received_at, before the
// cutoff). olderThan=0 disables only the age filter: a still-pending signal
// (unacked, attempts < MaxSignalAttempts) is never eligible for deletion
// regardless of olderThan — the structural gate protects it even on an
// unbounded sweep (deleting a pending row would strand it forever, since
// signal_cursors has already advanced past it).
func GCSignals(dbtx db.DBTX, olderThan time.Duration, dryRun bool) (int64, error) {
	cond := `(acked_at IS NOT NULL) OR (acked_at IS NULL AND attempts >= ?)`
	args := []any{MaxSignalAttempts}
	if olderThan > 0 {
		cutoff := formatSignalTime(time.Now().Add(-olderThan))
		cond = `(` + cond + `) AND ((acked_at IS NOT NULL AND acked_at < ?) OR (acked_at IS NULL AND received_at < ?))`
		args = append(args, cutoff, cutoff)
	}

	if dryRun {
		var n int64
		row := dbtx.QueryRow(`SELECT COUNT(*) FROM signals WHERE `+cond, args...)
		if err := row.Scan(&n); err != nil {
			return 0, fmt.Errorf("count signals: %w", err)
		}
		return n, nil
	}

	res, err := dbtx.Exec(`DELETE FROM signals WHERE `+cond, args...)
	if err != nil {
		return 0, fmt.Errorf("delete signals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete signals: rows affected: %w", err)
	}
	return n, nil
}

const signalSelectCols = `workspace_id, service, connector, id, occurred_at, identity, url, author, title, received_at, attempts, acked_at`

type signalRowScanner interface {
	Scan(dest ...any) error
}

func scanSignalRow(s signalRowScanner) (*Signal, error) {
	var sig Signal
	var occurredAt, receivedAt string
	var ackedAt sql.NullString
	if err := s.Scan(
		&sig.WorkspaceID, &sig.Service, &sig.Connector, &sig.ID,
		&occurredAt, &sig.Identity, &sig.URL, &sig.Author, &sig.Title,
		&receivedAt, &sig.Attempts, &ackedAt,
	); err != nil {
		return nil, err
	}
	t, err := parseSignalTime(occurredAt)
	if err != nil {
		return nil, fmt.Errorf("parse occurred_at: %w", err)
	}
	sig.OccurredAt = t
	rt, err := parseSignalTime(receivedAt)
	if err != nil {
		return nil, fmt.Errorf("parse received_at: %w", err)
	}
	sig.ReceivedAt = rt
	if ackedAt.Valid {
		at, err := parseSignalTime(ackedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse acked_at: %w", err)
		}
		sig.AckedAt = &at
	}
	return &sig, nil
}

func scanSignals(rows *sql.Rows) ([]*Signal, error) {
	out := []*Signal{}
	for rows.Next() {
		sig, err := scanSignalRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan signal: %w", err)
		}
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list signals: rows: %w", err)
	}
	return out, nil
}

// dedupeStringsPreserveOrder returns ss with duplicates removed, keeping
// first-seen order (AckSignals uses this so a caller-supplied id list with
// repeats doesn't turn into a duplicate-placeholder SQL query).
func dedupeStringsPreserveOrder(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

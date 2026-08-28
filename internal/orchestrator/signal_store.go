package orchestrator

// docs/plans/signal-ingest-detailed-design.md §2 (PR-1): signal inbox
// (migration 0046) の store 層。
//
// signal-driven-review.md §8.1 の inbox 不変条件 2 つをここで実装する:
//
//   - dedup は PRIMARY KEY そのもの: (workspace_id, service, connector, id)
//     の再投入は `INSERT OR IGNORE` で no-op になる (採点表 Q10)
//   - source cursor は処理済み Signal 自身を越えて前進する: IngestSignals
//     が INSERT と cursor 前進を同一 tx で行い (crash しても取りこぼさない
//     — 採点表 Q13)、cursor は現在値より小さい occurred_at には絶対に
//     戻らない (単調前進)
//
// occurred_at / cursor の比較は必ず RFC3339 を time.Time として parse して
// から行う — jira は `+09:00` のようなオフセット付きで occurred_at を返す
// ため、生文字列の比較では offset が混在した瞬間に大小関係が壊れる
// (2026-08-19T23:00:00-09:00 は実時刻で 2026-08-20T08:00:00Z だが、日付の
// 文字列だけを見ると「19日」で「20日」より小さく見える、が実例)。保存も
// 読み出しも UTC 正規化した RFC3339 で行う。
//
// 全メソッドが db.DBTX を直接取るプレーン関数である点は trigger_run.go /
// task_identity.go と同じ形。IngestSignals / ClaimSignals は複数の文で
// 1 つの不変条件を守る必要がある ("1 tx" — 挿入+cursor前進、select+
// attempts++) ため、TaskRepository 側のラッパーは DeleteTask と同じ
// 「渡された db が生の *sql.DB なら自分で InTxDB を張る」パターンを使う
// (repository.go 参照)。

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

// MaxSignalAttempts is the v0 constant retry ceiling before a signal is
// considered dead (design doc §2: "無限再配送の防止...v0 は定数 5"). Store
// functions take maxAttempts as an explicit parameter rather than baking
// this in directly, so tests can exercise the dead-letter boundary without
// waiting through 5 real claims — but every non-test caller should pass
// this constant.
const MaxSignalAttempts = 5

// DefaultSignalListLimit / MaxSignalListLimit bound ListSignals/ClaimSignals
// row counts, mirroring action_list.go's DefaultActionListLimit/
// MaxActionListLimit (same "row-count cap, not response-size cap" posture —
// see that file's doc comment for the full rationale, which applies
// identically here since this is also a brand-new read surface).
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
	// row is not deleted (no "stuck mid-ingestion" terminal state,
	// signal-driven-review.md §8.1), just excluded from ClaimSignals /
	// HasPendingSignals until a human acks it or GC eventually reaps it.
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

// SignalIngestRow is one Signal as read from a connector's JSONL output
// (signal-ingest-detailed-design.md §5.3 / §7.1 envelope): 1 line =
// `{id, occurred_at, identity, url?, author?, title?}`. ID/OccurredAt/
// Identity are required (§7.2: "JSONL の必須 field"); URL/Author/Title
// default to "" when omitted. json tags match the envelope field names
// exactly, since these are also what gets marshaled for the per-row
// MaxContentBytes size check (§2).
type SignalIngestRow struct {
	ID         string `json:"id"`
	OccurredAt string `json:"occurred_at"` // RFC3339, tz-aware
	Identity   string `json:"identity"`
	URL        string `json:"url,omitempty"`
	Author     string `json:"author,omitempty"`
	Title      string `json:"title,omitempty"`
}

// SignalFilter scopes and paginates ListSignals. WorkspaceID is required —
// mirroring ActionListFilter's ErrActionListUnscoped posture (action_list.go),
// this is a workspace-wide read surface with no "give me everything"
// fallback.
type SignalFilter struct {
	WorkspaceID string
	Service     string
	Connector   string
	// State defaults to SignalStatePending when empty (matching `boid
	// signal list`'s "未 ack の Signal" default framing, signal-driven-
	// review.md §8.2).
	State SignalState
	Limit int
	// MaxAttempts, when <= 0, defaults to MaxSignalAttempts. Exposed as a
	// filter field (not hardcoded) for the same testability reason
	// MaxSignalAttempts' own doc comment gives.
	MaxAttempts int
}

func resolveMaxAttempts(maxAttempts int) int {
	if maxAttempts <= 0 {
		return MaxSignalAttempts
	}
	return maxAttempts
}

// signalTimeLayout is a FIXED-WIDTH RFC3339-with-nanoseconds layout used for
// every stored timestamp (occurred_at/received_at/acked_at/cursor/
// updated_at).
//
// [M2, Opus review 2026-08-26]: this is deliberately NOT time.RFC3339Nano.
// RFC3339Nano trims trailing zeros from the fractional-second component,
// producing a VARIABLE-width string whose lexicographic order does not
// always match chronological order — e.g. "...T01:00:00.5Z" (with a
// fractional second) sorts BEFORE "...T01:00:00Z" (no fractional second at
// all) because '.' (0x2E) < 'Z' (0x5A), even though the first instant is
// LATER. ListSignals/ClaimSignals both `ORDER BY occurred_at` as a plain
// TEXT column, so lexicographic order matching chronological order is load-
// bearing, not cosmetic — a fixed 9-digit fractional field (zero-padded,
// never trimmed) is what makes that hold for every stored row.
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
// each row it INSERT OR IGNOREs into signals (PK-based dedup, Q10) and then
// advances (workspace, service, connector)'s cursor to
// max(rows.OccurredAt) — but ONLY forward, never backward (a batch that is
// entirely older than the current cursor leaves it untouched).
//
// Callers MUST invoke this within a single transaction (dbtx must be a
// db.DBTX backed by an open *sql.Tx, or the whole call must itself be the
// only statement running against a *sql.DB) — the INSERTs and the cursor
// UPDATE/INSERT are only atomic as a unit if dbtx guarantees it. See
// TaskRepository.IngestSignals (repository.go) for the wrapper that
// provides this guarantee when given a raw *sql.DB, and this file's own
// package doc comment for why (Q13: crash between the inserts and the
// cursor advance must not lose signals OR leave the cursor pointing past
// unsaved rows).
func IngestSignals(dbtx db.DBTX, workspaceID, service, connector string, rows []SignalIngestRow) error {
	if workspaceID == "" {
		return fmt.Errorf("ingest signals: workspace id must not be empty")
	}
	// service MAY be empty (docs/plans/boid-internal-signal-inbox.md §4.6):
	// boid's own internal-signal source (InternalSignalPack/
	// InternalSignalConnector, signal_ingest_bridge.go) never reaches an
	// external service, so its envelope's source.service is deliberately
	// "" — unlike workspaceID/connector, which every source (external or
	// internal) always has, service has no fallback value that wouldn't
	// misrepresent "no service instance was involved" as some real one.
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
// or "" if the source has never ingested (design doc §2: "無ければ空文字
// (= 最初から)").
//
// Contract (design doc §5.3): the cursor is EXCLUSIVE of itself from the
// connector's point of view — a conformant connector drops anything with
// `occurred_at <= cursor` before ever calling `boid signal ingest`, and
// treats `occurred_at > cursor` as new. This store does not enforce or
// interpret that filtering itself (PK-based dedup in IngestSignals is a
// second, independent safety net for exact-duplicate re-sends; a
// well-behaved connector's own `<= cursor` filtering is what keeps fetch
// volume down) — GetSignalCursor just returns the opaque stored value.
//
// [L8, Opus review 2026-08-26] KNOWN RISK inherited from this
// exclusive-boundary design (same bug family as khi's pre-rewrite
// ts-based bookmarks — see the boid-project memory note
// "khi-source-bookmark-must-be-exclusive"): if two sibling events share the
// EXACT SAME occurred_at and the cursor advances to that timestamp after
// only one of them has been ingested (the other arriving from the source in
// a later page/fetch), the connector's own `occurred_at <= cursor`
// self-filter will silently drop the sibling FOREVER — it never reaches
// IngestSignals, so PK-based dedup never even sees it to no-op it. This is
// not something the store layer can fix (a cursor here is a single scalar,
// not a set of "ids already seen at this timestamp"). Pack authors (§7.2)
// working with sub-second or tie-prone sources should add a secondary
// tiebreaker (e.g. sort by id too) in their own fetch logic rather than
// relying on occurred_at alone to be unique.
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
// selected — this is the "no infinite redelivery" guard (design doc §2,
// khi's MAX_ATTEMPTS equivalent).
//
// Like IngestSignals, this does a select then a write that must be one
// atomic unit — see IngestSignals' own doc comment for the transaction
// contract callers must uphold, and TaskRepository.ClaimSignals for the
// wrapper that provides it automatically.
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

	unique := dedupeStringsPreserveOrder(ids)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")

	existing := map[string]bool{}
	args := make([]any, 0, len(unique)+1)
	args = append(args, workspaceID)
	for _, id := range unique {
		args = append(args, id)
	}
	rows, err := dbtx.Query(`SELECT DISTINCT id FROM signals WHERE workspace_id = ? AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		return fmt.Errorf("ack signals: lookup: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("ack signals: scan: %w", err)
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("ack signals: rows: %w", err)
	}
	rows.Close()

	var unknown []string
	for _, id := range unique {
		if !existing[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("ack signals: unknown id(s) in workspace %q: %s", workspaceID, strings.Join(unknown, ", "))
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
// predicate (design doc §4.2). Dead signals never count: a workspace with
// only dead-lettered signals must not fire its trigger forever
// (signal-driven-review.md §8.1).
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

// GCSignals deletes signals rows that are STRUCTURALLY eligible — acked
// (judgment complete) or dead-lettered (retries exhausted, attempts >=
// MaxSignalAttempts) — and, when olderThan > 0, additionally old enough
// (acked_at, respectively received_at, before the cutoff). olderThan=0
// disables the AGE filter only, matching GCTasks (status IN (...)) /
// GCTriggerRuns (finished_at IS NOT NULL): those siblings keep a
// non-time STATE predicate that protects live rows even when the caller
// asks for an unbounded sweep, and GCSignals now does the same — a
// still-pending signal (unacked, attempts < MaxSignalAttempts) is NEVER
// eligible for deletion, regardless of olderThan.
//
// [H1, Opus review 2026-08-26, CONFIRMED data-loss bug]: an earlier version
// of this function had no structural gate — at olderThan<=0 its WHERE
// clause degenerated to `1 = 1`, deleting EVERY signal in the table,
// including brand-new pending (attempts=0) rows that had never even been
// claimed once. Worse, signal_cursors was left pointing PAST the deleted
// rows (IngestSignals had already advanced it), so the connector's own
// "occurred_at <= cursor, drop it" self-filter (§5.3) would never let those
// rows be re-ingested — permanent, silent loss. Reachable via
// `boid gc --older-than 0`, `POST /api/gc {"older_than":"0s"}`, or the 24h
// auto-GC loop if `gc.older_than` were ever configured to 0. The structural
// gate below closes this: a pending row can never match, so it survives
// every combination of olderThan and dryRun.
//
// Intended to run inside the SAME transaction as GCTasks/GCTriggerRuns
// (TaskGCStore.GC, repository.go) — a single statement, so it needs no tx
// guarantee of its own the way IngestSignals/ClaimSignals do.
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

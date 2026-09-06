package orchestrator

// card_requests 台帳の store 層: card 上の一つの実行要求 (人発コマンド/Go、
// または内部イベント発の自動要求) を queued → launching → attached →
// finished/failed で管理する。folded は複数の queued 行を一回の起動に
// まとめたときの、代表に選ばれなかった行の一時状態。
//
// card_id ごとに launching/attached は一つ、という制約は
// idx_card_requests_active_unique が DB 側で保証する。実際に `run:` を
// 起動する launcher 本体や上位の実行枠判定への統合はここでは扱わない。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/db"
)

// CardRequestStatus is card_requests.status's closed vocabulary — mirrors
// migration 0049's CHECK (status IN (...)).
type CardRequestStatus string

const (
	CardRequestStatusQueued    CardRequestStatus = "queued"
	CardRequestStatusLaunching CardRequestStatus = "launching"
	CardRequestStatusAttached  CardRequestStatus = "attached"
	CardRequestStatusFolded    CardRequestStatus = "folded"
	CardRequestStatusFinished  CardRequestStatus = "finished"
	CardRequestStatusFailed    CardRequestStatus = "failed"
)

// CardRequestCommandKeyGo is the reserved command_key value for a request
// whose origin is the shared work-execution slot (Go) rather than a
// project.yaml card_commands entry. ValidateCardCommands rejects an empty
// card_commands key at load time, so this dedicated sentinel never collides
// with a real one.
//
// This is deliberately non-empty: ReconcileLaunchingCardRequests
// (card_request_release.go) uses CommandKey == CardRequestCommandKeyGo as
// its skip predicate for "this row has no real launcher job, don't
// self-heal it". Only RunCardCommandAsHuman (always a real, non-empty
// card_commands key) and acceptGo (this sentinel) create card_requests rows
// today, so an empty CommandKey never occurs in practice — but if that ever
// changed, a plain "" sentinel would silently also match a row that never
// meant to claim Go's exemption. A dedicated non-empty value keeps "" free
// to mean exactly what CardRequest's own zero value implies (no command_key
// set at all), rather than double-booking it as a magic marker.
const CardRequestCommandKeyGo = "__go__"

// CardRequestTargetKind vocabulary — the kind of continuation a launcher
// created.
const (
	CardRequestTargetKindTask    = "task"
	CardRequestTargetKindSession = "session"
)

// CardRequestDefinition is the command definition snapshotted onto a
// request the instant it transitions queued → launching, so a later
// project.yaml edit cannot retroactively change what this request ran.
type CardRequestDefinition struct {
	CommandKey string
	Label      string
	Run        string
	// Version is an opaque caller-supplied marker (e.g. a hash of
	// label+run) callers may use to detect definition drift; the store
	// itself never interprets it.
	Version string
}

// CardRequest is one row of the card_requests table.
type CardRequest struct {
	ID         string
	CardID     string
	CommandKey string
	// CauseID's emptiness IS the origin signal downstream (empty = human,
	// non-empty = internal event — see cardContextResponse.Origin and
	// BoidOpAgentStart's event rejection). Every event-caused creation path
	// MUST set a non-empty CauseID, or it becomes indistinguishable from a
	// human request; this store cannot enforce that on a caller's behalf.
	CauseID string
	Status  CardRequestStatus
	// Instruction is the user's free-text input for this request. It lives
	// on the row from creation and stays untouched by
	// ClaimQueuedCardRequests/RetryCardRequest (those only ever reset the
	// launch-time snapshot), so a queued/folded request's input survives
	// until it is actually claimed.
	Instruction   string
	Launched      CardRequestDefinition
	LauncherJobID string
	TargetKind    string
	TargetID      string
	FoldedInto    string
	Result        string
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

var (
	// ErrCardRequestNotFound: no card_requests row with the given id.
	ErrCardRequestNotFound = errors.New("card request: not found")
	// ErrCardRequestSlotOccupied: idx_card_requests_active_unique rejected
	// giving a card a second launching-or-attached row.
	ErrCardRequestSlotOccupied = errors.New("card request: card's single execution slot is already occupied")
	// ErrCardRequestDuplicateCause: idx_card_requests_cause_unique rejected
	// a cause_id already recorded on another row.
	ErrCardRequestDuplicateCause = errors.New("card request: cause id already recorded (redelivery)")
	// ErrCardRequestInvalidTransition: the row is not in a status the
	// requested transition accepts from.
	ErrCardRequestInvalidTransition = errors.New("card request: invalid status transition")
	// ErrNoQueuedCardRequests is ClaimQueuedCardRequests' sentinel for "this
	// card has nothing queued right now".
	ErrNoQueuedCardRequests = errors.New("card request: no queued requests for card")
)

// CreateCardRequest inserts a new card_requests row. req.ID is generated
// when empty. req.Status must be "" (defaults to queued) or
// CardRequestStatusLaunching, for a caller that already knows the slot is
// free and wants to claim it in the same INSERT.
//
// Returns ErrCardRequestSlotOccupied or ErrCardRequestDuplicateCause when
// the corresponding unique index rejects the insert.
func CreateCardRequest(dbtx db.DBTX, req *CardRequest) error {
	if req.CardID == "" {
		return fmt.Errorf("create card request: card id must not be empty")
	}
	switch req.Status {
	case "":
		req.Status = CardRequestStatusQueued
	case CardRequestStatusQueued:
		// allowed starting state
	case CardRequestStatusLaunching:
		// The launching fast path claims the slot in the same INSERT, so it
		// must carry its launcher's job id atomically too — the same
		// invariant ClaimQueuedCardRequests enforces for the queued->launching
		// path below. A row must never be launching with no launcher of
		// record (see BoidOpAgentStart's ownership check).
		if req.LauncherJobID == "" {
			return fmt.Errorf("create card request: launching fast path requires a launcher job id")
		}
	default:
		return fmt.Errorf("create card request: invalid starting status %q", req.Status)
	}
	if req.ID == "" {
		req.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if req.CreatedAt.IsZero() {
		req.CreatedAt = now
	}
	req.CreatedAt = req.CreatedAt.UTC()
	req.UpdatedAt = now

	_, err := dbtx.Exec(
		`INSERT INTO card_requests (
			id, card_id, command_key, cause_id, status, instruction,
			launched_command_key, launched_label, launched_run, launched_version,
			launcher_job_id, target_kind, target_id, folded_into, result, error,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.CardID, req.CommandKey, req.CauseID, string(req.Status), req.Instruction,
		req.Launched.CommandKey, req.Launched.Label, req.Launched.Run, req.Launched.Version,
		req.LauncherJobID, req.TargetKind, req.TargetID, req.FoldedInto, req.Result, req.Error,
		req.CreatedAt, req.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: card_requests.card_id") {
			return ErrCardRequestSlotOccupied
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed: card_requests.cause_id") {
			return ErrCardRequestDuplicateCause
		}
		return fmt.Errorf("create card request: %w", err)
	}
	return nil
}

// ClaimQueuedCardRequests snapshots every currently-queued request for
// cardID (oldest first), promotes the oldest to launching (claiming the
// card's shared slot) while atomically stamping launcherJobID as its
// owner, and folds the rest into it (status=folded, folded_into=primary.id).
// A request created after this snapshot is not part of it and stays queued
// for the next claim.
//
// launcherJobID must be non-empty: stamping it in the SAME statement that
// promotes the row to launching is what closes the window where a row could
// sit launching with no launcher of record (see BoidOpAgentStart's ownership
// check, which now rejects an empty LauncherJobID rather than treating it as
// unclaimed).
//
// Returns ErrNoQueuedCardRequests when cardID has no queued requests.
// Returns ErrCardRequestSlotOccupied if another row for this card is
// already launching/attached.
//
// Must be called within a transaction for atomicity (multiple statements).
func ClaimQueuedCardRequests(dbtx db.DBTX, cardID, launcherJobID string, def CardRequestDefinition) (primary *CardRequest, folded []*CardRequest, err error) {
	if cardID == "" {
		return nil, nil, fmt.Errorf("claim queued card requests: card id must not be empty")
	}
	if launcherJobID == "" {
		return nil, nil, fmt.Errorf("claim queued card requests: launcher job id must not be empty")
	}
	rows, err := dbtx.Query(
		cardRequestSelectCols+` FROM card_requests WHERE card_id = ? AND status = ? ORDER BY created_at ASC, id ASC`,
		cardID, string(CardRequestStatusQueued),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("claim queued card requests: list: %w", err)
	}
	pending, err := scanCardRequests(rows)
	if err != nil {
		return nil, nil, fmt.Errorf("claim queued card requests: %w", err)
	}
	if len(pending) == 0 {
		return nil, nil, ErrNoQueuedCardRequests
	}

	now := time.Now().UTC()
	head := pending[0]
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, launched_command_key = ?, launched_label = ?, launched_run = ?, launched_version = ?, launcher_job_id = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		string(CardRequestStatusLaunching), def.CommandKey, def.Label, def.Run, def.Version, launcherJobID, now,
		head.ID, string(CardRequestStatusQueued),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: card_requests.card_id") {
			return nil, nil, ErrCardRequestSlotOccupied
		}
		return nil, nil, fmt.Errorf("claim queued card requests: promote %q: %w", head.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, fmt.Errorf("claim queued card requests: %q was no longer queued: %w", head.ID, ErrCardRequestInvalidTransition)
	}
	head.Status = CardRequestStatusLaunching
	head.Launched = def
	head.LauncherJobID = launcherJobID
	head.UpdatedAt = now

	rest := pending[1:]
	for _, r := range rest {
		if _, err := dbtx.Exec(
			`UPDATE card_requests SET status = ?, folded_into = ?, updated_at = ? WHERE id = ? AND status = ?`,
			string(CardRequestStatusFolded), head.ID, now, r.ID, string(CardRequestStatusQueued),
		); err != nil {
			return nil, nil, fmt.Errorf("claim queued card requests: fold %q: %w", r.ID, err)
		}
		r.Status = CardRequestStatusFolded
		r.FoldedInto = head.ID
		r.UpdatedAt = now
	}
	return head, rest, nil
}

// AttachCardRequest records the continuation (task or session) a launcher
// created, transitioning launching → attached. Only valid from launching;
// attaching twice is rejected rather than re-pointing the target.
func AttachCardRequest(dbtx db.DBTX, id, targetKind, targetID string) error {
	if id == "" {
		return fmt.Errorf("attach card request: id must not be empty")
	}
	if targetKind != CardRequestTargetKindTask && targetKind != CardRequestTargetKindSession {
		return fmt.Errorf("attach card request: invalid target kind %q", targetKind)
	}
	if targetID == "" {
		return fmt.Errorf("attach card request: target id must not be empty")
	}
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, target_kind = ?, target_id = ?, updated_at = ? WHERE id = ? AND status = ?`,
		string(CardRequestStatusAttached), targetKind, targetID, time.Now().UTC(), id, string(CardRequestStatusLaunching),
	)
	if err != nil {
		return fmt.Errorf("attach card request: %w", err)
	}
	return rowsAffectedOrNotFoundOrInvalid(dbtx, res, id)
}

// FinishCardRequest records a request's successful outcome — attached →
// finished — and closes out two related sets of rows in the same call:
// every request folded into id, and every OLDER still-failed request for
// the same card (a later success absorbs an earlier failure). "Older" is
// bounded by id's own created_at: id is always the oldest member of its own
// claim boundary (ClaimQueuedCardRequests promotes the oldest queued row),
// so a failed request created after id must belong to a LATER boundary this
// success never actually read, and is deliberately left alone.
//
// Only valid from attached. Must be called within a transaction for
// atomicity (a read plus three UPDATEs).
func FinishCardRequest(dbtx db.DBTX, id, result string) error {
	if id == "" {
		return fmt.Errorf("finish card request: id must not be empty")
	}
	row := dbtx.QueryRow(`SELECT card_id, status, created_at FROM card_requests WHERE id = ?`, id)
	var cardID, status string
	var createdAt time.Time
	if err := row.Scan(&cardID, &status, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("finish card request %q: %w", id, ErrCardRequestNotFound)
		}
		return fmt.Errorf("finish card request: %w", err)
	}
	if CardRequestStatus(status) != CardRequestStatusAttached {
		return fmt.Errorf("finish card request %q: was %q, not attached: %w", id, status, ErrCardRequestInvalidTransition)
	}

	now := time.Now().UTC()
	absorbedResult := "absorbed by " + id
	if _, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, result = ?, updated_at = ? WHERE id = ?`,
		string(CardRequestStatusFinished), result, now, id,
	); err != nil {
		return fmt.Errorf("finish card request: %w", err)
	}
	if _, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, result = ?, error = '', updated_at = ? WHERE folded_into = ? AND status = ?`,
		string(CardRequestStatusFinished), absorbedResult, now, id, string(CardRequestStatusFolded),
	); err != nil {
		return fmt.Errorf("finish card request: close folded requests: %w", err)
	}
	if _, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, result = ?, error = '', updated_at = ? WHERE card_id = ? AND status = ? AND created_at < ?`,
		string(CardRequestStatusFinished), absorbedResult, now, cardID, string(CardRequestStatusFailed), createdAt,
	); err != nil {
		return fmt.Errorf("finish card request: absorb failed requests: %w", err)
	}
	return nil
}

// FailCardRequest records a request's terminal failure from any
// non-terminal status, and releases anything folded into it back to
// queued so the next claim reconsiders them. The failed row itself is kept
// for inspection/retry, not deleted.
//
// Must be called within a transaction for atomicity (two UPDATEs).
func FailCardRequest(dbtx db.DBTX, id, errText string) error {
	if id == "" {
		return fmt.Errorf("fail card request: id must not be empty")
	}
	now := time.Now().UTC()
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, error = ?, updated_at = ? WHERE id = ? AND status IN (?, ?, ?)`,
		string(CardRequestStatusFailed), errText, now, id,
		string(CardRequestStatusQueued), string(CardRequestStatusLaunching), string(CardRequestStatusAttached),
	)
	if err != nil {
		return fmt.Errorf("fail card request: %w", err)
	}
	if err := rowsAffectedOrNotFoundOrInvalid(dbtx, res, id); err != nil {
		return err
	}
	if _, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, folded_into = '', updated_at = ? WHERE folded_into = ? AND status = ?`,
		string(CardRequestStatusQueued), now, id, string(CardRequestStatusFolded),
	); err != nil {
		return fmt.Errorf("fail card request: release folded requests: %w", err)
	}
	return nil
}

// RetryCardRequest re-queues a failed request explicitly. Only valid from
// failed; clears the error, the previous launch-time snapshot, and the
// previous attempt's continuation target (a stale target/result must not
// survive onto the fresh attempt).
func RetryCardRequest(dbtx db.DBTX, id string) error {
	if id == "" {
		return fmt.Errorf("retry card request: id must not be empty")
	}
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, error = '', launched_command_key = '', launched_label = '', launched_run = '', launched_version = '', launcher_job_id = '', target_kind = '', target_id = '', result = '', updated_at = ?
		 WHERE id = ? AND status = ?`,
		string(CardRequestStatusQueued), time.Now().UTC(), id, string(CardRequestStatusFailed),
	)
	if err != nil {
		return fmt.Errorf("retry card request: %w", err)
	}
	return rowsAffectedOrNotFoundOrInvalid(dbtx, res, id)
}

// CountActiveCardRequests returns the number of card_requests rows
// currently occupying cardID's shared execution slot (launching or
// attached). In practice 0 or 1, since the unique index caps it.
func CountActiveCardRequests(dbtx db.DBTX, cardID string) (int, error) {
	row := dbtx.QueryRow(
		`SELECT COUNT(*) FROM card_requests WHERE card_id = ? AND status IN (?, ?)`,
		cardID, string(CardRequestStatusLaunching), string(CardRequestStatusAttached),
	)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count active card requests: %w", err)
	}
	return n, nil
}

// GetCardRequest fetches a single card_requests row by id.
func GetCardRequest(dbtx db.DBTX, id string) (*CardRequest, error) {
	row := dbtx.QueryRow(cardRequestSelectCols+` FROM card_requests WHERE id = ?`, id)
	req, err := scanCardRequestRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get card request %q: %w", id, ErrCardRequestNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get card request: %w", err)
	}
	return req, nil
}

// ListCardRequestsByCard returns every card_requests row for cardID, oldest
// first — diagnostics / test inspection, not a hot path.
func ListCardRequestsByCard(dbtx db.DBTX, cardID string) ([]*CardRequest, error) {
	rows, err := dbtx.Query(cardRequestSelectCols+` FROM card_requests WHERE card_id = ? ORDER BY created_at ASC, id ASC`, cardID)
	if err != nil {
		return nil, fmt.Errorf("list card requests: %w", err)
	}
	return scanCardRequests(rows)
}

// ListActiveCardRequests returns every currently launching/attached
// card_requests row across EVERY card, oldest first — the bulk counterpart
// to ListCardRequestsByCard for a caller that needs "which cards have an
// active request" (e.g. `boid task diagnose-cards`) without issuing one
// query per card.
func ListActiveCardRequests(dbtx db.DBTX) ([]*CardRequest, error) {
	rows, err := dbtx.Query(
		cardRequestSelectCols+` FROM card_requests WHERE status IN (?, ?) ORDER BY created_at ASC, id ASC`,
		string(CardRequestStatusLaunching), string(CardRequestStatusAttached),
	)
	if err != nil {
		return nil, fmt.Errorf("list active card requests: %w", err)
	}
	return scanCardRequests(rows)
}

// rowsAffectedOrNotFoundOrInvalid turns a zero-rows-affected UPDATE result
// into ErrCardRequestNotFound (no such row) or ErrCardRequestInvalidTransition
// (row exists, wrong starting status).
func rowsAffectedOrNotFoundOrInvalid(dbtx db.DBTX, res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, gerr := GetCardRequest(dbtx, id); errors.Is(gerr, ErrCardRequestNotFound) {
		return gerr
	}
	return fmt.Errorf("card request %q: %w", id, ErrCardRequestInvalidTransition)
}

const cardRequestSelectCols = `SELECT id, card_id, command_key, cause_id, status, instruction,
	launched_command_key, launched_label, launched_run, launched_version,
	launcher_job_id, target_kind, target_id, folded_into, result, error,
	created_at, updated_at`

// cardRequestRowScanner is satisfied by both *sql.Row and *sql.Rows, same
// trick trigger_run.go's triggerRunRowScanner uses.
type cardRequestRowScanner interface {
	Scan(dest ...any) error
}

func scanCardRequestRow(row cardRequestRowScanner) (*CardRequest, error) {
	var r CardRequest
	var status string
	if err := row.Scan(
		&r.ID, &r.CardID, &r.CommandKey, &r.CauseID, &status, &r.Instruction,
		&r.Launched.CommandKey, &r.Launched.Label, &r.Launched.Run, &r.Launched.Version,
		&r.LauncherJobID, &r.TargetKind, &r.TargetID, &r.FoldedInto, &r.Result, &r.Error,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil, err
	}
	r.Status = CardRequestStatus(status)
	return &r, nil
}

func scanCardRequests(rows *sql.Rows) ([]*CardRequest, error) {
	defer rows.Close()
	out := []*CardRequest{}
	for rows.Next() {
		r, err := scanCardRequestRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan card request: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list card requests: rows: %w", err)
	}
	return out, nil
}

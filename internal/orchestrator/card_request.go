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
	"context"
	"database/sql"
	"encoding/json"
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
// card_commands key at load time, so this sentinel never collides with a
// real one; ReconcileLaunchingCardRequests uses it as its skip predicate.
const CardRequestCommandKeyGo = "__go__"

// CardRequestOriginHuman/Event are the two values a card_requests row's
// origin can report — see CardRequestOrigin.
const (
	CardRequestOriginHuman = "human"
	CardRequestOriginEvent = "event"
)

// CardRequestOrigin derives a card_requests row's origin from its cause_id
// — the one rule both `boid card context` (server.cardRequestOrigin) and
// this package's own self-record share, so a future change to it cannot
// update one call site and leave the other behind.
func CardRequestOrigin(causeID string) string {
	if causeID != "" {
		return CardRequestOriginEvent
	}
	return CardRequestOriginHuman
}

// ActionTypeCommandFinished/Failed/ForceReleased are the card action-log
// entries FinishCardRequest/FailCardRequest/ForceReleaseCardRequest
// self-record — see recordCardRequestOutcome.
const (
	ActionTypeCommandFinished      = "command_finished"
	ActionTypeCommandFailed        = "command_failed"
	ActionTypeCommandForceReleased = "command_force_released"
)

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
	// CardWrite is CardCommand.CardWrite, snapshotted the same instant as
	// Label/Run/Version. A CardRequestCommandKeyGo request never sets this.
	CardWrite bool
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
	// ErrCardRequestDuplicateCause: idx_card_requests_cause_unique_non_failed
	// rejected a cause_id already recorded on another non-failed row.
	ErrCardRequestDuplicateCause = errors.New("card request: cause id already recorded (redelivery)")
	// ErrCardRequestInvalidTransition: the row is not in a status the
	// requested transition accepts from.
	ErrCardRequestInvalidTransition = errors.New("card request: invalid status transition")
	// ErrNoQueuedCardRequests is ClaimQueuedCardRequests' sentinel for "this
	// card has nothing queued right now".
	ErrNoQueuedCardRequests = errors.New("card request: no queued requests for card")
	// ErrCardRequestOwnerMismatch: AttachCardRequestOwned's caller-asserted
	// launcher_job_id did not match the row's CURRENT owner (re-read after
	// the owner-scoped UPDATE affected zero rows) — a genuine ownership
	// violation, distinct from ErrCardRequestInvalidTransition (which also
	// covers the idempotent-retry case where the row is already correctly
	// attached to the SAME caller's own target). Callers must not treat this
	// as a retry-converges outcome.
	ErrCardRequestOwnerMismatch = errors.New("card request: caller is not the request's current owner")
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
			launched_command_key, launched_label, launched_run, launched_version, launched_card_write,
			launcher_job_id, target_kind, target_id, folded_into, result, error,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.CardID, req.CommandKey, req.CauseID, string(req.Status), req.Instruction,
		req.Launched.CommandKey, req.Launched.Label, req.Launched.Run, req.Launched.Version, req.Launched.CardWrite,
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
		`UPDATE card_requests SET status = ?, launched_command_key = ?, launched_label = ?, launched_run = ?, launched_version = ?, launched_card_write = ?, launcher_job_id = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		string(CardRequestStatusLaunching), def.CommandKey, def.Label, def.Run, def.Version, def.CardWrite, launcherJobID, now,
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

// validateAttachArgs is the argument validation AttachCardRequest and
// AttachCardRequestOwned share.
func validateAttachArgs(id, targetKind, targetID string) error {
	if id == "" {
		return fmt.Errorf("attach card request: id must not be empty")
	}
	if targetKind != CardRequestTargetKindTask && targetKind != CardRequestTargetKindSession {
		return fmt.Errorf("attach card request: invalid target kind %q", targetKind)
	}
	if targetID == "" {
		return fmt.Errorf("attach card request: target id must not be empty")
	}
	return nil
}

// AttachCardRequest records the continuation (task or session) a launcher
// created, transitioning launching → attached. Only valid from launching;
// attaching twice is rejected rather than re-pointing the target.
//
// No caller identity is asserted here — reserved for the reconcile/recovery
// paths (attachFoundContinuationOrFail) that establish the continuation's
// legitimacy some OTHER way (a DB-verified foreign-key match, not a
// caller-supplied claim). Any path that instead trusts a caller's own
// assertion of "I am the launcher" — BoidOpTaskCreate, BoidOpAgentStart —
// MUST use AttachCardRequestOwned instead, so that assertion is re-checked
// fresh in the same statement that writes, not read separately beforehand
// and trusted stale.
func AttachCardRequest(dbtx db.DBTX, id, targetKind, targetID string) error {
	if err := validateAttachArgs(id, targetKind, targetID); err != nil {
		return err
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

// AttachCardRequestOwned is AttachCardRequest with the caller's claimed
// launcher_job_id asserted directly in the UPDATE's WHERE clause, instead of
// trusting a separate, earlier GetCardRequest read of it. Closes the window
// where a force-release followed by a different launcher's reclaim, landing
// between that earlier read and this write, would otherwise let the stale
// caller's attach steal the new owner's slot (the row is "launching" again
// by then, just under a different launcher_job_id — a plain status check
// alone cannot tell the two apart).
//
// Returns ErrCardRequestOwnerMismatch when the row's CURRENT launcher_job_id
// differs from expectedLauncherJobID — a real ownership violation. Returns
// ErrCardRequestInvalidTransition (same as AttachCardRequest) for every
// other zero-rows-affected case, INCLUDING the row already being attached to
// expectedLauncherJobID's own earlier attach — callers rely on that
// distinction to keep an idempotent retry converging (re-read and compare
// target) without misclassifying it as a stolen slot.
func AttachCardRequestOwned(dbtx db.DBTX, id, expectedLauncherJobID, targetKind, targetID string) error {
	if expectedLauncherJobID == "" {
		return fmt.Errorf("attach card request: expected launcher job id must not be empty")
	}
	if err := validateAttachArgs(id, targetKind, targetID); err != nil {
		return err
	}
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, target_kind = ?, target_id = ?, updated_at = ? WHERE id = ? AND status = ? AND launcher_job_id = ?`,
		string(CardRequestStatusAttached), targetKind, targetID, time.Now().UTC(), id, string(CardRequestStatusLaunching), expectedLauncherJobID,
	)
	if err != nil {
		return fmt.Errorf("attach card request: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return nil
	}
	existing, gerr := GetCardRequest(dbtx, id)
	if gerr != nil {
		return gerr
	}
	if existing.LauncherJobID != expectedLauncherJobID {
		return fmt.Errorf("attach card request %q: owned by launcher %q, not %q: %w",
			id, existing.LauncherJobID, expectedLauncherJobID, ErrCardRequestOwnerMismatch)
	}
	return fmt.Errorf("attach card request %q: %w", id, ErrCardRequestInvalidTransition)
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
// Also self-records a command_finished action on the card's own action log
// (recordCardRequestTerminalOutcome) — see that function's own doc comment.
//
// Only valid from attached. Must be called within a transaction for
// atomicity (a read plus three UPDATEs, plus the self-record's own INSERT).
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
	// cause_id = '' only: a cause-bearing failed row stays failed rather
	// than being absorbed into finished, which would re-enter it into the
	// cause_id dedup index — colliding with whatever live row now carries
	// that same cause (the redelivery this row's own failure allowed).
	if _, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, result = ?, error = '', updated_at = ? WHERE card_id = ? AND status = ? AND created_at < ? AND cause_id = ''`,
		string(CardRequestStatusFinished), absorbedResult, now, cardID, string(CardRequestStatusFailed), createdAt,
	); err != nil {
		return fmt.Errorf("finish card request: absorb failed requests: %w", err)
	}
	return recordCardRequestTerminalOutcome(dbtx, id, ActionTypeCommandFinished, result, "")
}

// FailCardRequest records a request's terminal failure from any
// non-terminal status, and releases anything folded into it back to
// queued so the next claim reconsiders them (an automatic failure carries
// no intent to stop the card — contrast ForceReleaseCardRequest). The
// failed row itself is kept for inspection/retry, not deleted.
//
// Also self-records a command_failed action on the card's own action log
// (recordCardRequestTerminalOutcome) — see that function's own doc comment.
//
// Must be called within a transaction for atomicity (two UPDATEs, plus the
// self-record's own INSERT).
func FailCardRequest(dbtx db.DBTX, id, errText string) error {
	if _, err := failCardRequest(dbtx, id, errText, foldedSiblingsRequeue); err != nil {
		return err
	}
	return recordCardRequestTerminalOutcome(dbtx, id, ActionTypeCommandFailed, "", errText)
}

// CardRequestOutcomeSibling is one row a force-release also force-failed —
// the payload shape for CardRequestOutcomePayload.ForceFailedSiblings.
type CardRequestOutcomeSibling struct {
	ID         string `json:"id"`
	CommandKey string `json:"command_key"`
}

// CardRequestOutcomePayload is the JSON shape every card_requests self-record
// action (command_finished/command_failed/command_force_released) writes.
// Exported so a reader (the card timeline read model) can parse it back
// without duplicating this wire shape — see ParseCardRequestOutcomePayload.
type CardRequestOutcomePayload struct {
	RequestID           string                      `json:"request_id"`
	CommandKey          string                      `json:"command_key"`
	LaunchedLabel       string                      `json:"launched_label"`
	LauncherJobID       string                      `json:"launcher_job_id"`
	TargetKind          string                      `json:"target_kind"`
	TargetID            string                      `json:"target_id"`
	Origin              string                      `json:"origin"`
	CauseID             string                      `json:"cause_id"`
	Result              string                      `json:"result"`
	Error               string                      `json:"error"`
	Reason              string                      `json:"reason,omitempty"`
	ForceFailedSiblings []CardRequestOutcomeSibling `json:"force_failed_siblings,omitempty"`
}

// ParseCardRequestOutcomePayload decodes a command_finished/command_failed/
// command_force_released action's payload. Returns the zero value and no
// error for empty/malformed input — a reader must never fail to render a
// card's timeline over one unparseable historical row.
func ParseCardRequestOutcomePayload(payload json.RawMessage) (CardRequestOutcomePayload, error) {
	var p CardRequestOutcomePayload
	if len(payload) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return CardRequestOutcomePayload{}, err
	}
	return p, nil
}

// recordCardRequestTerminalOutcome self-records id's finished/failed outcome
// — see recordCardRequestOutcome, which this and ForceReleaseCardRequest's
// own self-record both funnel through.
func recordCardRequestTerminalOutcome(dbtx db.DBTX, id, actionType, result, errText string) error {
	return recordCardRequestOutcome(dbtx, id, actionType, result, errText, "", nil)
}

// recordCardRequestOutcome self-records id's terminal outcome onto its
// card's own action log via CreateAction, so the command's history survives
// GCCardRequests deleting the card_requests row itself. Skipped for a
// CardRequestCommandKeyGo row — the shared work-execution slot's own
// outcome already self-records via child_closed, and a second record here
// would duplicate it.
//
// Re-reads id fresh (rather than taking pre-computed fields) so the payload
// reflects exactly what the caller's own terminal UPDATE just persisted.
func recordCardRequestOutcome(dbtx db.DBTX, id, actionType, result, errText, reason string, siblings []ForceReleasedSibling) error {
	row, err := GetCardRequest(dbtx, id)
	if err != nil {
		return fmt.Errorf("record card request outcome: reload %q: %w", id, err)
	}
	if row.CommandKey == CardRequestCommandKeyGo {
		return nil
	}
	p := CardRequestOutcomePayload{
		RequestID:     row.ID,
		CommandKey:    row.CommandKey,
		LaunchedLabel: row.Launched.Label,
		LauncherJobID: row.LauncherJobID,
		TargetKind:    row.TargetKind,
		TargetID:      row.TargetID,
		Origin:        CardRequestOrigin(row.CauseID),
		CauseID:       row.CauseID,
		Result:        result,
		Error:         errText,
		Reason:        reason,
	}
	for _, s := range siblings {
		p.ForceFailedSiblings = append(p.ForceFailedSiblings, CardRequestOutcomeSibling(s))
	}
	payload, merr := json.Marshal(p)
	if merr != nil {
		return fmt.Errorf("record card request outcome: marshal payload: %w", merr)
	}
	action := &Action{
		TaskID:  row.CardID,
		Type:    actionType,
		Payload: payload,
		Actor:   ActorDaemon,
	}
	if err := CreateAction(context.Background(), dbtx, action, nil, nil); err != nil {
		return fmt.Errorf("record card request outcome: create action: %w", err)
	}
	return nil
}

// foldedSiblingOutcome selects what failCardRequest does to id's folded
// siblings once id itself is marked failed.
type foldedSiblingOutcome int

const (
	// foldedSiblingsRequeue returns each folded sibling to queued.
	foldedSiblingsRequeue foldedSiblingOutcome = iota
	// foldedSiblingsFail marks each folded sibling failed instead.
	foldedSiblingsFail
)

// ForceReleasedSibling identifies one folded card_requests row a
// foldedSiblingsFail pass force-failed alongside the row a caller named.
type ForceReleasedSibling struct {
	ID         string
	CommandKey string
}

func failCardRequest(dbtx db.DBTX, id, errText string, siblings foldedSiblingOutcome) ([]ForceReleasedSibling, error) {
	if id == "" {
		return nil, fmt.Errorf("fail card request: id must not be empty")
	}
	now := time.Now().UTC()
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, error = ?, updated_at = ? WHERE id = ? AND status IN (?, ?, ?)`,
		string(CardRequestStatusFailed), errText, now, id,
		string(CardRequestStatusQueued), string(CardRequestStatusLaunching), string(CardRequestStatusAttached),
	)
	if err != nil {
		return nil, fmt.Errorf("fail card request: %w", err)
	}
	if err := rowsAffectedOrNotFoundOrInvalid(dbtx, res, id); err != nil {
		return nil, err
	}
	switch siblings {
	case foldedSiblingsFail:
		// Read the siblings before failing them — the UPDATE below clears
		// folded_into, so this is the only chance to report which rows (and
		// their command_key) got swept up.
		folded, ferr := listFoldedCardRequests(dbtx, id)
		if ferr != nil {
			return nil, fmt.Errorf("fail card request: list folded requests: %w", ferr)
		}
		siblingErrText := fmt.Sprintf("folded into %s, which failed: %s", id, errText)
		if _, err := dbtx.Exec(
			`UPDATE card_requests SET status = ?, folded_into = '', error = ?, updated_at = ? WHERE folded_into = ? AND status = ?`,
			string(CardRequestStatusFailed), siblingErrText, now, id, string(CardRequestStatusFolded),
		); err != nil {
			return nil, fmt.Errorf("fail card request: fail folded requests: %w", err)
		}
		return folded, nil
	default:
		if _, err := dbtx.Exec(
			`UPDATE card_requests SET status = ?, folded_into = '', updated_at = ? WHERE folded_into = ? AND status = ?`,
			string(CardRequestStatusQueued), now, id, string(CardRequestStatusFolded),
		); err != nil {
			return nil, fmt.Errorf("fail card request: release folded requests: %w", err)
		}
		return nil, nil
	}
}

// listFoldedCardRequests returns the id/command_key of every row currently
// folded into id, oldest first.
func listFoldedCardRequests(dbtx db.DBTX, id string) ([]ForceReleasedSibling, error) {
	rows, err := dbtx.Query(
		`SELECT id, command_key FROM card_requests WHERE folded_into = ? AND status = ? ORDER BY created_at ASC`,
		id, string(CardRequestStatusFolded),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForceReleasedSibling
	for rows.Next() {
		var s ForceReleasedSibling
		if err := rows.Scan(&s.ID, &s.CommandKey); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RetryCardRequest re-queues a failed request explicitly. Only valid from
// failed; clears the error, the previous launch-time snapshot, and the
// previous attempt's continuation target (a stale target/result must not
// survive onto the fresh attempt).
//
// Also clears any force-release barrier on the request's card
// (ClearCardForceReleaseBarrier) — an explicit Retry lifts automatic-
// dispatch suppression the same as a fresh human command or Go.
func RetryCardRequest(dbtx db.DBTX, id string) error {
	if id == "" {
		return fmt.Errorf("retry card request: id must not be empty")
	}
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, error = '', launched_command_key = '', launched_label = '', launched_run = '', launched_version = '', launched_card_write = FALSE, launcher_job_id = '', target_kind = '', target_id = '', result = '', updated_at = ?
		 WHERE id = ? AND status = ?`,
		string(CardRequestStatusQueued), time.Now().UTC(), id, string(CardRequestStatusFailed),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: card_requests.cause_id") {
			return ErrCardRequestDuplicateCause
		}
		return fmt.Errorf("retry card request: %w", err)
	}
	if err := rowsAffectedOrNotFoundOrInvalid(dbtx, res, id); err != nil {
		return err
	}
	row, gerr := GetCardRequest(dbtx, id)
	if gerr != nil {
		return fmt.Errorf("retry card request: reload for barrier: %w", gerr)
	}
	return ClearCardForceReleaseBarrier(dbtx, row.CardID)
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

// GetCardRequestByTaskTarget finds the card_requests row whose continuation
// is taskID, if any. Returns (nil, nil) — not an error — when taskID is not
// a card-command continuation, so a caller can treat "no card context" as
// the ordinary case rather than special-casing a sentinel error.
func GetCardRequestByTaskTarget(dbtx db.DBTX, taskID string) (*CardRequest, error) {
	row := dbtx.QueryRow(
		cardRequestSelectCols+` FROM card_requests WHERE target_kind = ? AND target_id = ? ORDER BY created_at DESC LIMIT 1`,
		CardRequestTargetKindTask, taskID,
	)
	req, err := scanCardRequestRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get card request by task target: %w", err)
	}
	return req, nil
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
	launched_command_key, launched_label, launched_run, launched_version, launched_card_write,
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
		&r.Launched.CommandKey, &r.Launched.Label, &r.Launched.Run, &r.Launched.Version, &r.Launched.CardWrite,
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

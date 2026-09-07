package timeline

// Card read model: a second, parallel timeline builder for a card's own
// action log (child_added/child_specced/child_closed/attrs_set/noted/
// wake_due/command_finished/...), which Build's execution-detail filtering
// drops entirely. Kept in package timeline (not a new package) so it can
// reuse Build's own "orchestrator-only, importable by web/templates"
// property directly.
//
// Unlike Build (a pure function over caller-resolved inputs), the functions
// here take a db.DBTX and read the card's full action history themselves,
// grouping it into items in memory rather than paging raw actions from the
// database in batches. The wire cursor format (EncodeActionCursor's
// (created_at, id) keyset, applied over items rather than actions) does not
// depend on this choice.
import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// CardItemKind enumerates the kinds of item a card's timeline read model
// returns.
type CardItemKind string

const (
	// CardItemChild is one work child, tracked by its stable
	// TaskTriageChild.ID from spec through execution to closure, sitting at
	// its child_added (creation) position.
	CardItemChild CardItemKind = "child"
	// CardItemChildFinished is the lightweight pointer item a closed child
	// leaves at ITS OWN closing position, linking back to CardItemChild via
	// CorrelationID.
	CardItemChildFinished CardItemKind = "child_finished"
	// CardItemCommand is a card command's self-recorded terminal outcome, or
	// — when Pinned — the card command currently running.
	CardItemCommand CardItemKind = "command"
	// CardItemSuggestion is one attrs_set action whose payload carries a
	// "suggestion" key.
	CardItemSuggestion CardItemKind = "suggestion"
	// CardItemAnswered is one "answered" action.
	CardItemAnswered CardItemKind = "answered"
	// CardItemSummary is one attrs_set action whose payload carries a
	// "summary" key.
	CardItemSummary CardItemKind = "summary"
	// CardItemNote is one "noted" action.
	CardItemNote CardItemKind = "note"
	// CardItemWakeDue is one "wake_due" action.
	CardItemWakeDue CardItemKind = "wake_due"
)

// CardChildDetail is CardItem's payload for Kind == CardItemChild (both the
// rich creation-position item and its lightweight CardItemChildFinished
// pointer share the same *CardChildDetail — see loadCardTimelineState).
type CardChildDetail struct {
	ChildID string
	Title   string
	// Status is one of orchestrator.TaskTriageChildStatus*.
	Status  string
	Spec    *orchestrator.TaskTriageChildSpec
	TaskRef string
	// TaskExists reports whether TaskRef still resolves to a live task row —
	// false once the child's own task has been GC'd (GCTasks, 30 days after
	// terminal). A caller must not render a link when this is false.
	TaskExists bool
	// ClosingActionType is "child_closed" (the child's own task reached
	// done/aborted) or "child_dropped" (withdrawn before ever being
	// dispatched) once Status is closed; "" while still open/specced/
	// dispatched.
	ClosingActionType string
	// HasResult, Result, ResultStatus are populated from the child_closed
	// action's own payload (never from a live task lookup), so they survive
	// the child task row's own GC. Only set when ClosingActionType is
	// "child_closed" — a child_dropped closure carries no result.
	HasResult    bool
	Result       string
	ResultStatus orchestrator.TaskStatus
	// LiveStatus is the dispatched child's live orchestrator.TaskStatus
	// (executing/awaiting/done/aborted). This package never sets it — it is
	// a caller-side enrichment; "" means unresolved.
	LiveStatus string
}

// CardCommandDetail is CardItem's payload for Kind == CardItemCommand. The
// same struct shape covers both a still-running command (Pinned, Outcome
// == "") and a terminal history entry (Outcome one of finished/failed/
// force_released).
type CardCommandDetail struct {
	RequestID  string
	CommandKey string
	// Label is the launch-time snapshot (card_requests.launched_label) —
	// empty for a still-queued (never-launched) pinned request.
	Label string
	// Instruction is best-effort: read from the LIVE card_requests row when
	// it still exists. Once GCCardRequests has removed that row, this is
	// simply "" — only Result/Error/Reason (sourced from the terminal
	// action's own payload) are guaranteed to survive that GC.
	Instruction string
	// Origin is "human" or "event" (orchestrator.CardRequestOrigin).
	Origin     string
	CauseID    string
	TargetKind string
	TargetID   string
	// TargetExists is resolved only for TargetKind == "task" (checked via a
	// live task lookup) — false once that task has been GC'd. Left false,
	// not "unknown", for TargetKind == "session": a session's liveness is a
	// job-table concern the existing job pages already own, out of scope
	// here.
	TargetExists bool
	// Status is the live card_requests.status (queued/launching/attached),
	// set only while Pinned — a renderer needs this to distinguish "not
	// dispatched yet" from "running" (a queued request has no target, no
	// Label, and would otherwise be indistinguishable from a launching
	// one). Empty for a terminal history item, where Outcome is the
	// authoritative status instead.
	Status orchestrator.CardRequestStatus
	// Outcome is "" while Pinned (still running), else one of "finished" /
	// "failed" / "force_released".
	Outcome string
	Result  string
	Error   string
	Reason  string
}

// CardItem is one row of a card's timeline read model. Kind selects which of
// Action/Child/Command is populated. ID is a raw actions.id for every
// action-backed kind, or a deterministic derived string for the two cases
// that don't map 1:1 onto one action (a suggestion/summary sharing one
// attrs_set action gets "<action id>:suggestion" / "<action id>:summary"; a
// still-pinned, not-yet-launched command has no action yet and gets
// "pending-command:<request id>").
//
// CorrelationID is a relationship key, never an identity: it lets a UI link
// a CardItemChildFinished back to its CardItemChild (both carry the child's
// stable TaskTriageChild.ID). A pinned CardItemCommand leaves it empty — its
// pinned and terminal forms are never displayed at once, so nothing needs
// linking.
type CardItem struct {
	ID   string
	Kind CardItemKind
	// Time is the item's position, straight from actions.created_at (UTC).
	// This package does no timezone conversion — a renderer calls .Local()
	// at format time, the same way web/templates/tasks.templ already does
	// for the execution-detail timeline.
	Time          time.Time
	HasTime       bool
	CorrelationID string
	// Pinned marks an item BuildCardTimeline deliberately excludes
	// (CardPinnedItems returns it instead) because it is still the current
	// occupant of a fixed slot. Once resolved, the same ID reappears from
	// BuildCardTimeline at its normal chronological position.
	Pinned bool

	// Action is the backing action for every kind except CardItemChild (and
	// a still-pinned CardItemCommand, which has none yet).
	Action *orchestrator.Action
	// Child is populated only for Kind == CardItemChild.
	Child *CardChildDetail
	// Command is populated only for Kind == CardItemCommand.
	Command *CardCommandDetail
}

// DefaultCardTimelineLimit / MaxCardTimelineLimit bound one BuildCardTimeline
// call's ITEM count, mirroring orchestrator.ClampActionListLimit's role for
// the raw action list.
const (
	DefaultCardTimelineLimit = 10
	MaxCardTimelineLimit     = 200
)

// ClampCardTimelineLimit resolves a caller-requested limit (<=0 meaning "use
// the default") against the two constants above.
func ClampCardTimelineLimit(requested int) int {
	if requested <= 0 {
		return DefaultCardTimelineLimit
	}
	if requested > MaxCardTimelineLimit {
		return MaxCardTimelineLimit
	}
	return requested
}

// CardTimelinePage is one page of BuildCardTimeline's result.
type CardTimelinePage struct {
	Items []CardItem
	// NextCursor: pass to the next BuildCardTimeline call's cursor argument
	// to load older items ("Load older"). Unchanged from the input cursor
	// when Items is empty (same "nothing new, keep resending" contract as
	// orchestrator.ListActionsSince).
	NextCursor string
	// HasMore reports whether older items exist beyond this page.
	HasMore bool
}

// cardTimelineState is everything BuildCardTimeline and CardPinnedItems both
// derive from, computed exactly once so a pinned item and its eventual
// historical counterpart always agree on ID/Time/content — see CardItem's
// own doc comment on Pinned.
type cardTimelineState struct {
	items []CardItem
	// activeCommand is the single pinned "currently running command" item,
	// if any — kept separate from items because (unlike a pinned child or
	// suggestion) it has no possible historical counterpart sharing its ID
	// (a command's pinned and terminal forms are never the same item — see
	// CardCommandDetail's own doc comment).
	activeCommand *CardItem
}

// BuildCardTimeline returns cardID's history, newest-first, paginated by
// ITEM (not raw action) via a keyset cursor over (item time, item id) —
// EncodeActionCursor/DecodeActionCursor's own format, reused verbatim.
// Items currently pinned are excluded — see CardPinnedItems.
func BuildCardTimeline(dbtx db.DBTX, cardID, cursor string, limit int) (*CardTimelinePage, error) {
	sinceTime, sinceID, err := orchestrator.DecodeActionCursor(cursor)
	if err != nil {
		return nil, err
	}
	st, err := loadCardTimelineState(dbtx, cardID)
	if err != nil {
		return nil, err
	}

	history := make([]CardItem, 0, len(st.items))
	for _, it := range st.items {
		if it.Pinned {
			continue
		}
		history = append(history, it)
	}
	sortCardItemsDesc(history)

	if !sinceTime.IsZero() || sinceID != "" {
		filtered := history[:0:0]
		for _, it := range history {
			if isOlderThanCursor(it.Time, it.ID, sinceTime, sinceID) {
				filtered = append(filtered, it)
			}
		}
		history = filtered
	}

	limit = ClampCardTimelineLimit(limit)
	hasMore := len(history) > limit
	if hasMore {
		history = history[:limit]
	}

	nextCursor := cursor
	if len(history) > 0 {
		last := history[len(history)-1]
		nextCursor = orchestrator.EncodeActionCursor(last.Time, last.ID)
	}
	return &CardTimelinePage{Items: history, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// CardPinnedItems returns cardID's currently-pinned items: the live
// suggestion, the next unresolved work spec/child, and the sole in-progress
// task/session (a dispatched child, or a running card command). Uses the
// SAME CardItem type/IDs BuildCardTimeline would eventually show once
// resolved.
func CardPinnedItems(dbtx db.DBTX, cardID string) ([]CardItem, error) {
	st, err := loadCardTimelineState(dbtx, cardID)
	if err != nil {
		return nil, err
	}
	pinned := make([]CardItem, 0, len(st.items))
	for _, it := range st.items {
		if it.Pinned {
			pinned = append(pinned, it)
		}
	}
	if st.activeCommand != nil {
		pinned = append(pinned, *st.activeCommand)
	}
	sortCardItemsDesc(pinned)
	return pinned, nil
}

// isOlderThanCursor reports whether (t, id) belongs on the NEXT ("older")
// page after (sinceTime, sinceID) — the id tie-break (matching
// sortCardItemsDesc) is what keeps same-instant items from being skipped or
// re-delivered across a page boundary.
func isOlderThanCursor(t time.Time, id string, sinceTime time.Time, sinceID string) bool {
	if t.Before(sinceTime) {
		return true
	}
	if t.After(sinceTime) {
		return false
	}
	return id < sinceID
}

// sortCardItemsDesc sorts items newest-first, tie-broken by ID descending —
// the exact inverse comparison isOlderThanCursor uses to select the next
// page, so the two stay consistent by construction.
func sortCardItemsDesc(items []CardItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.After(b.Time)
		}
		return a.ID > b.ID
	})
}

// loadCardTimelineState reads cardID's full action history plus its live
// task_triage detail and card_requests rows, and derives every timeline
// item (pinned and historical) in one pass. See this file's package doc
// comment for why this reads the whole history rather than paging raw
// actions from the database.
func loadCardTimelineState(dbtx db.DBTX, cardID string) (*cardTimelineState, error) {
	var detail json.RawMessage
	if tt, err := orchestrator.GetTaskTriage(dbtx, cardID); err == nil {
		detail = tt.Detail
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	actions, err := orchestrator.ListActionsByTask(dbtx, cardID)
	if err != nil {
		return nil, err
	}
	children, err := orchestrator.DetailChildren(detail)
	if err != nil {
		return nil, err
	}
	requests, err := orchestrator.ListCardRequestsByCard(dbtx, cardID)
	if err != nil {
		return nil, err
	}

	st := &cardTimelineState{}

	childAnchor := map[string]*orchestrator.Action{}
	closingByTaskRef := map[string]*orchestrator.Action{}
	closingByChildID := map[string]*orchestrator.Action{}
	var suggestionActions []*orchestrator.Action
	var commandOutcomes []cardCommandOutcome

	for _, a := range actions {
		switch a.Type {
		case "child_added", "child_specced":
			// child_specced is update-only (applyChildSpeccedSideEffect
			// requires the child to already exist), so in practice it never
			// wins this — kept as a defensive fallback for a child entry
			// seeded some other way (a legacy row, or one created directly
			// in the child's own detail rather than through child_added).
			if id := payloadString(a.Payload, "id"); id != "" {
				if _, ok := childAnchor[id]; !ok {
					childAnchor[id] = a
				}
			}
		case "child_closed":
			if ref := payloadString(a.Payload, "child_id"); ref != "" {
				closingByTaskRef[ref] = a
			}
		case "child_dropped":
			if id := payloadString(a.Payload, "id"); id != "" {
				closingByChildID[id] = a
			}
		case "attrs_set":
			if hasPayloadKey(a.Payload, "suggestion") {
				suggestionActions = append(suggestionActions, a)
			}
		case orchestrator.ActionTypeCommandFinished, orchestrator.ActionTypeCommandFailed, orchestrator.ActionTypeCommandForceReleased:
			payload, _ := orchestrator.ParseCardRequestOutcomePayload(a.Payload)
			commandOutcomes = append(commandOutcomes, cardCommandOutcome{action: a, payload: payload})
		}
	}

	activeSuggestionActionID := ""
	if len(suggestionActions) > 0 {
		if _, ok := orchestrator.DetailSuggestion(detail); ok {
			activeSuggestionActionID = suggestionActions[len(suggestionActions)-1].ID
		}
	}

	requestsByID := make(map[string]*orchestrator.CardRequest, len(requests))
	for _, r := range requests {
		requestsByID[r.ID] = r
	}
	activeRequest := orchestrator.PickActiveCardRequest(requests)

	// One batched existence check for every task id this pass will need
	// (child TaskRefs, command targets) instead of one query per id.
	var taskIDsToCheck []string
	for i := range children {
		taskIDsToCheck = append(taskIDsToCheck, children[i].TaskRef)
	}
	for _, co := range commandOutcomes {
		if co.payload.TargetKind == orchestrator.CardRequestTargetKindTask {
			taskIDsToCheck = append(taskIDsToCheck, co.payload.TargetID)
		}
	}
	if activeRequest != nil && activeRequest.TargetKind == orchestrator.CardRequestTargetKindTask {
		taskIDsToCheck = append(taskIDsToCheck, activeRequest.TargetID)
	}
	existingTasks, err := orchestrator.ExistingTaskIDs(dbtx, taskIDsToCheck)
	if err != nil {
		return nil, err
	}

	for i := range children {
		c := &children[i]
		closed := c.Status == orchestrator.TaskTriageChildStatusClosed

		cd := &CardChildDetail{ChildID: c.ID, Title: c.Title, Status: c.Status, TaskRef: c.TaskRef}
		if c.Spec != nil {
			specCopy := *c.Spec
			cd.Spec = &specCopy
		}
		if c.TaskRef != "" {
			cd.TaskExists = existingTasks[c.TaskRef]
		}

		var closingAction *orchestrator.Action
		if c.TaskRef != "" {
			closingAction = closingByTaskRef[c.TaskRef]
		} else {
			closingAction = closingByChildID[c.ID]
		}
		if closingAction != nil {
			cd.ClosingActionType = closingAction.Type
			if closingAction.Type == "child_closed" {
				summary, status := parseChildClosedPayload(closingAction.Payload)
				cd.HasResult = true
				cd.Result = summary
				cd.ResultStatus = status
			}
		}

		anchor := childAnchor[c.ID]
		item := CardItem{
			Kind:          CardItemChild,
			CorrelationID: c.ID,
			Child:         cd,
			Pinned:        !closed,
		}
		if anchor != nil {
			item.ID = anchor.ID
			item.Time = anchor.CreatedAt
			item.HasTime = !anchor.CreatedAt.IsZero()
		} else {
			// Defensive fallback for a child with no anchor action at all
			// (see the child_specced comment above) — synthesize a stable,
			// deterministic id so the child is still visible somewhere
			// rather than silently dropped. Pinned is left as computed
			// above (!closed): a closed child must still return to
			// history, just at a synthetic, un-timestamped position.
			item.ID = "child:" + c.ID
		}
		st.items = append(st.items, item)

		if closed && closingAction != nil {
			st.items = append(st.items, CardItem{
				ID:            closingAction.ID,
				Kind:          CardItemChildFinished,
				Time:          closingAction.CreatedAt,
				HasTime:       !closingAction.CreatedAt.IsZero(),
				CorrelationID: c.ID,
				Action:        closingAction,
				Child:         cd,
			})
		}
	}

	for _, a := range actions {
		switch a.Type {
		case "attrs_set":
			if hasPayloadKey(a.Payload, "suggestion") {
				st.items = append(st.items, CardItem{
					ID:      a.ID + ":suggestion",
					Kind:    CardItemSuggestion,
					Time:    a.CreatedAt,
					HasTime: !a.CreatedAt.IsZero(),
					Action:  a,
					Pinned:  a.ID == activeSuggestionActionID,
				})
			}
			if hasPayloadKey(a.Payload, "summary") {
				st.items = append(st.items, CardItem{
					ID:      a.ID + ":summary",
					Kind:    CardItemSummary,
					Time:    a.CreatedAt,
					HasTime: !a.CreatedAt.IsZero(),
					Action:  a,
				})
			}
		case "answered":
			st.items = append(st.items, CardItem{ID: a.ID, Kind: CardItemAnswered, Time: a.CreatedAt, HasTime: !a.CreatedAt.IsZero(), Action: a})
		case "noted":
			st.items = append(st.items, CardItem{ID: a.ID, Kind: CardItemNote, Time: a.CreatedAt, HasTime: !a.CreatedAt.IsZero(), Action: a})
		case "wake_due":
			st.items = append(st.items, CardItem{ID: a.ID, Kind: CardItemWakeDue, Time: a.CreatedAt, HasTime: !a.CreatedAt.IsZero(), Action: a})
		}
	}
	for _, co := range commandOutcomes {
		st.items = append(st.items, buildCommandHistoryItem(co, requestsByID, existingTasks))
	}

	if activeRequest != nil {
		targetExists := activeRequest.TargetKind == orchestrator.CardRequestTargetKindTask && existingTasks[activeRequest.TargetID]
		st.activeCommand = &CardItem{
			ID:            "pending-command:" + activeRequest.ID,
			Kind:          CardItemCommand,
			Time:          activeRequest.CreatedAt,
			HasTime:       true,
			CorrelationID: activeRequest.ID,
			Pinned:        true,
			Command: &CardCommandDetail{
				RequestID:    activeRequest.ID,
				CommandKey:   activeRequest.CommandKey,
				Label:        activeRequest.Launched.Label,
				Instruction:  activeRequest.Instruction,
				Origin:       orchestrator.CardRequestOrigin(activeRequest.CauseID),
				CauseID:      activeRequest.CauseID,
				TargetKind:   activeRequest.TargetKind,
				TargetID:     activeRequest.TargetID,
				TargetExists: targetExists,
				Status:       activeRequest.Status,
			},
		}
	}

	return st, nil
}

// cardCommandOutcome pairs a command_finished/command_failed/
// command_force_released action with its already-parsed payload, so a
// single pass over the action list can also collect the task ids that need
// a batched existence check before any CardItem gets built.
type cardCommandOutcome struct {
	action  *orchestrator.Action
	payload orchestrator.CardRequestOutcomePayload
}

// buildCommandHistoryItem builds a terminal CardItemCommand from one
// command_finished/command_failed/command_force_released outcome. Every
// field comes from the action's own payload except the best-effort
// Instruction enrichment (requestsByID, from the card's still-live
// card_requests rows) and TargetExists (existingTasks, the batched check).
func buildCommandHistoryItem(co cardCommandOutcome, requestsByID map[string]*orchestrator.CardRequest, existingTasks map[string]bool) CardItem {
	a, p := co.action, co.payload
	outcome := "finished"
	switch a.Type {
	case orchestrator.ActionTypeCommandFailed:
		outcome = "failed"
	case orchestrator.ActionTypeCommandForceReleased:
		outcome = "force_released"
	}
	instruction := ""
	if live, ok := requestsByID[p.RequestID]; ok {
		instruction = live.Instruction
	}
	return CardItem{
		ID:            a.ID,
		Kind:          CardItemCommand,
		Time:          a.CreatedAt,
		HasTime:       !a.CreatedAt.IsZero(),
		CorrelationID: p.RequestID,
		Action:        a,
		Command: &CardCommandDetail{
			RequestID:    p.RequestID,
			CommandKey:   p.CommandKey,
			Label:        p.LaunchedLabel,
			Instruction:  instruction,
			Origin:       p.Origin,
			CauseID:      p.CauseID,
			TargetKind:   p.TargetKind,
			TargetID:     p.TargetID,
			TargetExists: p.TargetKind == orchestrator.CardRequestTargetKindTask && existingTasks[p.TargetID],
			Outcome:      outcome,
			Result:       p.Result,
			Error:        p.Error,
			Reason:       p.Reason,
		},
	}
}

// parseChildClosedPayload extracts a child_closed action's own summary/
// status fields — the GC-survivable source for a closed child's result,
// since the child's own task row may already be gone by the time this
// renders.
func parseChildClosedPayload(payload json.RawMessage) (summary string, status orchestrator.TaskStatus) {
	var p struct {
		ChildStatus string `json:"child_status"`
		Summary     string `json:"summary"`
	}
	if len(payload) == 0 {
		return "", ""
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.Summary, orchestrator.TaskStatus(p.ChildStatus)
}

// payloadString extracts one string field from a JSON object payload,
// returning "" for missing/malformed input — a read model must never fail
// to render over one unparseable historical row.
func payloadString(payload json.RawMessage, key string) string {
	if len(payload) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// hasPayloadKey reports whether a JSON object payload carries a non-null
// value under key.
func hasPayloadKey(payload json.RawMessage, key string) bool {
	if len(payload) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	raw, ok := m[key]
	if !ok {
		return false
	}
	return len(raw) > 0 && string(raw) != "null"
}

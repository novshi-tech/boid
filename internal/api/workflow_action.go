package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SideEffectConsumesPayload names the action types whose own side effect
// already consumes the action payload, so ApplyAction must NOT additionally
// merge that payload into the task's own — see the merge site below for what
// the pollution looks like.
//
// Exported, and a package-level var rather than a literal inside ApplyAction,
// so a test can compare it against the copy of this set the boid-metaproject
// skill's python client carries (boid_store.py's PAYLOAD_CONSUMING) — that
// client refuses to send a payload with a type that would be merged and
// cannot ask the daemon at call time, so the two sides must be kept in sync;
// TestSideEffectConsumesPayload_MatchesMetaprojectClient is what enforces it.
var SideEffectConsumesPayload = map[string]bool{
	"park":          true,
	"attrs_set":     true,
	"child_added":   true,
	"child_specced": true,
	"child_dropped": true,
	"noted":         true,
}

// isShutdownErr reports whether the dispatch failure was caused by the
// dispatch context being canceled (daemon shutdown). Checks both the ctx
// directly and the error chain so wrapped child-ctx cancellations are
// covered.
func isShutdownErr(ctx context.Context, err error) bool {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	return errors.Is(err, context.Canceled)
}

// applyActionOptions carries behavior only in-package callers can ask for.
// Not on ApplyActionRequest, which is the wire type every external entry point
// (HTTP, Web UI, brokered action_send, `boid action send`) decodes into — none
// of them may choose this.
type applyActionOptions struct {
	// actionPayloadOnly records req.Payload on the ACTION and keeps it out of
	// the task's own payload.
	//
	// The generic merge below is right for an agent describing its work, and
	// wrong for the daemon describing why it intervened: a `{code, message}`
	// abort reason merged into a task whose payload already has a `message`
	// silently replaces it — the same pollution SideEffectConsumesPayload
	// exists to prevent. abortOnDispatchError avoids it by writing the action
	// directly and never touching the task payload; this flag is how a caller
	// that still wants the state machine's legality check gets the same
	// guarantee.
	actionPayloadOnly bool
}

func (s *TaskWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	return s.applyAction(ctx, taskID, req, applyActionOptions{})
}

func (s *TaskWorkflowService) applyAction(ctx context.Context, taskID string, req ApplyActionRequest, opts applyActionOptions) (*ActionApplication, error) {
	// action_send's single implementation — HTTP API, Web UI, brokered
	// action_send, and `boid action send` all funnel through ApplyAction, so
	// checking the payload size cap here once covers all of them. Deliberately
	// ahead of the task load below: it needs neither the task nor a machine,
	// so there is no reason to make an oversized-payload caller pay for a DB
	// round trip first.
	if err := orchestrator.ValidateContentSize("action payload", req.Payload); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		// statusErrorForGetTaskErr, not a blanket 404: mapping every read
		// failure to "no such task" makes a busy/locked database
		// indistinguishable from a deleted row, and callers do branch on that
		// — SweepTriggers' timeout handling reads a 4xx as "this task will
		// never accept an abort" and ends the round without it, leaving the
		// task running with single-flight released, which is the failure that
		// whole path exists to prevent.
		return nil, statusErrorForGetTaskErr(err)
	}

	// The machine is selected per task (card vs execution) so it can only be
	// resolved once the task is loaded; machineFor is a pure function of
	// task.Type and cannot fail.
	sm := machineFor(task)

	// A caller still sending a card verb's retired spelling ("working"/
	// "done" — a workspace script or khi during a short compat window) is
	// normalized to its current name ("start"/"complete") BEFORE anything
	// below ever inspects req.Type — IsManualAction, the push-down defense,
	// and sm.Apply only ever need to
	// know the current spelling. Scoped to the card machine only: the
	// execution machine's own "start"/"done" verbs are untouched.
	if sm.Name == orchestrator.CardMachineName {
		req.Type = orchestrator.NormalizeCardVerb(req.Type)
	}

	// Only Manual:true rules on the resolved machine are reachable through
	// this public entry point. Manual:false rules — job_failed (applied
	// directly by CompleteJob), progress/done_request/fail_request
	// (non-transitioning records NotifyTask writes directly),
	// child_dispatched/child_closed (self-recorded bookkeeping), and
	// wake_due (recorded internally by SweepWake) — must never be settable
	// by an external caller. Checking the machine's own Manual flag instead
	// of a hand-maintained blocklist means a future non-manual rule can't be
	// forgotten the same way a past one was.
	if !sm.IsManualAction(req.Type) {
		return nil, &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("action %q is not available through this endpoint", req.Type),
		}
	}

	// A card-lifecycle transition action (go/working/park/drop/done/reopen —
	// orchestrator.IsCardTransitionAction) may only be applied by a human.
	// accept(verb)'s own internal re-entry into ApplyAction
	// (WebAppService.AnswerSuggestion) reuses the SAME request ctx the
	// human's accept click established, so it is never blocked by this check.
	// Non-transitioning card actions (attrs_set/child_added/child_specced/
	// child_dropped/noted/answered) are untouched.
	if sm.Name == orchestrator.CardMachineName && orchestrator.IsCardTransitionAction(req.Type) && orchestrator.ActorFromContext(ctx) != orchestrator.ActorHuman {
		return nil, &StatusError{
			Code:    http.StatusForbidden,
			Message: fmt.Sprintf("action %q is a card-lifecycle transition and may only be applied by a human (Web UI / CLI) or via accepting a suggestion", req.Type),
		}
	}

	// "go" bypasses the rest of this generic pipeline entirely: unlike the
	// other five card-lifecycle verbs (working/park/drop/done/reopen), which
	// are plain transitions the generic Tx flow below already handles fine,
	// go must task-ify any specced children BEFORE its parked→working
	// transition can commit (acceptGo's own doc comment, workflow_card.go —
	// that ordering cannot fit inside the one Tx this function opens further
	// down: SetMaxOpenConns(1) deadlocks a nested TaskCreator.CreateTask
	// call). This applies identically whether "go" arrived as a direct human
	// click (this line) or via accept(go) (applyAnswered below, which calls
	// acceptGo too) — both are the same verb with the same meaning. The
	// `false` here is acceptGo's own viaAccept param — a direct click, unlike
	// accept(go), still needs to strip AND audit-record an unrelated stale
	// suggestion it happens to supersede.
	if req.Type == "go" {
		return s.acceptGo(ctx, taskID, false)
	}
	// "answered" also bypasses this pipeline entirely — its own accept path
	// needs the identical non-Tx-then-Tx shape whenever the accepted verb is
	// "go", plus the read-then-apply-then-strip sequencing every other verb
	// requires. See applyAnswered's own doc comment (suggestion_accept.go)
	// for the full accept/reject implementation, including the separate
	// actor==human check on accept specifically (this guard above does not
	// cover it: "answered" itself is non-transitioning and not in
	// IsCardTransitionAction's set, since reject must stay reachable from any
	// actor).
	if req.Type == "answered" {
		return s.applyAnswered(ctx, taskID, req.Payload)
	}

	// Hydrate with workspace.yaml so kit-supplied hooks / env / capabilities
	// — and the workspace's default project definition (task_behaviors /
	// base_branch / fork_point / default_task_behavior) — are visible to the
	// dispatch loop.
	//
	// On hydration failure the two registration shapes diverge:
	//
	//   - a project.yaml-less project (task.ProjectID carries
	//     orchestrator.URLDerivedProjectIDPrefix) has NO task_behaviors of
	//     its own — every one of them comes from the workspace default
	//     merge this hydrate step performs. Falling back to the bare cached
	//     meta here would silently proceed with a meta that has ZERO
	//     behaviors defined, producing a dispatch failure with no visible
	//     connection to "workspace hydration failed", so it is treated as a
	//     hard error instead of a silent degrade.
	//   - an ordinary project.yaml-bearing project only loses the
	//     workspace-supplied extras (kit env / host_commands / the
	//     workspace-default merge, if any) — its own task_behaviors are
	//     unaffected, so the existing silent-fallback-to-bare-Get behavior
	//     is kept, with a logged warning so there is at least a paper trail
	//     when debugging a dispatch that looks like it's missing kit-level
	//     config.
	//
	// isNoYAMLProjectWithNoBehaviors additionally catches the "hydration
	// technically succeeded but degraded" window: GetWithWorkspace returns
	// (meta, nil error) — success — when the project has no linked
	// workspace, the workspace store is not configured, or workspace.yaml is
	// simply absent. None of those merge the workspace default in, so a
	// no-YAML project's meta comes back with the same ZERO task_behaviors
	// its bare cached meta always has. A (meta, nil) check alone would let
	// that degraded-but-"successful" case silently through to the exact
	// zero-behavior dispatch this whole guard exists to prevent.
	isNoYAMLProjectWithNoBehaviors := func(m *orchestrator.ProjectMeta) bool {
		return orchestrator.IsURLDerivedProjectID(task.ProjectID) && (m == nil || len(m.TaskBehaviors) == 0)
	}

	var meta *orchestrator.ProjectMeta
	hydrated, hydrateErr := s.Meta.GetWithWorkspace(ctx, task.ProjectID)
	switch {
	case hydrateErr == nil && hydrated != nil && !isNoYAMLProjectWithNoBehaviors(hydrated):
		meta = hydrated
	case orchestrator.IsURLDerivedProjectID(task.ProjectID):
		detail := hydrateErr
		if detail == nil {
			detail = errors.New("workspace hydration reported success but produced no task_behaviors (no workspace linked, workspace store not configured, or workspace.yaml not found)")
		}
		return nil, &StatusError{
			Code: http.StatusInternalServerError,
			Message: fmt.Sprintf(
				"project %q has no project.yaml (its task_behaviors come entirely from the workspace default definition) and workspace hydration did not supply any, so dispatch cannot proceed: %v",
				task.ProjectID, detail,
			),
		}
	default:
		var ok bool
		meta, ok = s.Meta.Get(task.ProjectID)
		if !ok {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
		}
		slog.Warn("workspace hydration failed; dispatching with bare project meta (workspace-supplied env/host_commands/default project definition not applied)",
			"project_id", task.ProjectID, "task_id", taskID, "error", hydrateErr)
	}

	fromStatus := task.Status
	action := &orchestrator.Action{
		TaskID:  task.ID,
		Type:    req.Type,
		Payload: req.Payload,
		Actor:   orchestrator.ActorFromContext(ctx),
	}
	// sm.Apply alone has no rule admitting attrs_set against a done task —
	// this pre-Tx call routes through resolveAttrsSetDoneTransition's
	// service-layer guard first (see its own doc comment, attrs_set_done.go),
	// which falls straight through to sm.Apply for every other combination.
	var getTriage func(string) (*orchestrator.CardAttrs, error)
	if s.TaskTriage != nil {
		getTriage = s.TaskTriage.GetTaskTriage
	}
	newTask, statusErr := resolveAttrsSetDoneTransition(sm, task, action, getTriage)
	if statusErr != nil {
		return nil, statusErr
	}
	action.FromStatus = fromStatus
	action.ToStatus = newTask.Status

	// reopen carries an optional `{"instruction": {...}}` payload that appends a
	// new entry to the task's instruction history. The instruction is recorded
	// only on the action (audit trail) and not merged into task.payload.
	var reopenPayloadConsumed bool
	// Instructions is execution-only — a card's "reopen" (done/dropped →
	// parked) has no instruction history to append to at all, so this whole
	// block is scoped to newTask.Exec != nil. A card reopen carrying an
	// instruction-override payload simply has nothing to consume it
	// (reopenPayloadConsumed stays false).
	if req.Type == "reopen" && len(req.Payload) > 0 && newTask.Exec != nil {
		var p struct {
			Instruction *orchestrator.Instruction `json:"instruction,omitempty"`
		}
		if err := json.Unmarshal(req.Payload, &p); err == nil && p.Instruction != nil {
			inst := *p.Instruction
			if active := newTask.Exec.Instructions.Active(); active != nil {
				if inst.Agent == "" {
					inst.Agent = active.Agent
				}
				if inst.Model == "" {
					inst.Model = active.Model
				}
			}
			newTask.Exec.Instructions = orchestrator.AppendInstruction(newTask.Exec.Instructions, inst)
			reopenPayloadConsumed = true
		}
	}

	// park / attrs_set / child_added / child_specced / noted all feed their
	// payload exclusively into a task_triage.detail side-effect (below) and
	// the actions table's own audit-trail row — never into task.Payload
	// (which the CLI/Web UI surface wholesale). noted has no task_triage
	// side effect that "consumes" the payload the way attrs_set's fold does
	// — its payload is an opaque workspace-defined blob with no relationship
	// to task.Payload at all — but merging it in would be the same
	// pollution this map exists to prevent for the other four. ("answered"
	// used to be in this map too — it now bypasses this entire function via
	// applyAnswered, the early redirect above.)

	// Payload is execution-only — a card has no field to merge action.Payload
	// into at all, so this generic merge is scoped to newTask.Exec != nil.
	if !opts.actionPayloadOnly && !reopenPayloadConsumed && !SideEffectConsumesPayload[req.Type] && newTask.Exec != nil {
		merged, err := orchestrator.MergePayload(newTask.Exec.Payload, action.Payload)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "payload merge: " + err.Error()}
		}
		newTask.Exec.Payload = merged
	}

	// park / attrs_set / child_added / child_specced / child_dropped each
	// validate their payload before the transaction opens, so a malformed
	// payload surfaces as 400 rather than being swallowed into WithinTx's
	// generic 500 wrapping.
	var parkPayloadParsed *parkPayload
	var attrsSetParsed *attrsSetPatch
	var childAddedParsed *childAddedPayload
	var childSpeccedParsed *childSpeccedPayload
	var childDroppedParsed *childDroppedPayload
	// suggestionVerbChanged is set inside the Tx below by
	// applyAttrsSetSideEffect's own return value — declared here, at
	// function scope, so it survives past the switch's `return` inside the
	// Tx closure for the post-commit notifySuggestionArrived gate further
	// down (notifySuggestionArrived's own doc comment, queue_notify.go,
	// explains why "did the verb actually change" — not merely "was a verb
	// present" — is the correct notify trigger).
	var suggestionVerbChanged bool
	switch req.Type {
	case "park":
		var perr error
		parkPayloadParsed, perr = parseParkPayload(req.Payload)
		if perr != nil {
			return nil, perr
		}
	case "attrs_set":
		var perr error
		attrsSetParsed, perr = parseAttrsSetPayload(req.Payload)
		if perr != nil {
			return nil, perr
		}
	case "child_added":
		var perr error
		childAddedParsed, perr = parseChildAddedPayload(req.Payload)
		if perr != nil {
			return nil, perr
		}
	case "child_specced":
		var perr error
		childSpeccedParsed, perr = parseChildSpeccedPayload(req.Payload)
		if perr != nil {
			return nil, perr
		}
	case "child_dropped":
		var perr error
		childDroppedParsed, perr = parseChildDroppedPayload(req.Payload)
		if perr != nil {
			return nil, perr
		}
	case "noted":
		// The daemon never interprets noted's payload — parseNotedPayload
		// only confirms it is syntactically valid JSON.
		if perr := parseNotedPayload(req.Payload); perr != nil {
			return nil, perr
		}
	}

	// attrs_set/child_added/child_specced/child_dropped/noted are
	// non-transitioning (ToStatus=="" in the matched rule) AND their payload
	// is fully consumed by the side-effect above (never merged into
	// task.Payload either) — so the task row's STATUS genuinely has nothing
	// new to persist. Skipping tx.UpdateTask(newTask) for exactly these
	// closes a race: a caller read `task` before opening this Tx, so
	// `newTask` is built from that stale snapshot; if a REAL transition
	// (e.g. working→parked) committed concurrently in between, an
	// unconditional UpdateTask(newTask) here would silently stomp the fresh
	// status back to the stale one the non-transitioning action happened to
	// read, even though its own side-effect (the sidecar RMW) correctly
	// read-modified-wrote inside this same Tx. park is NOT in this set — it
	// genuinely transitions status, so it still needs the write.
	//
	// "nothing new to persist" stopped being globally true once updated_at
	// became a signal worth persisting on its own — a suggestion attaching
	// to an attrs_set-only action (below, gated on suggestionVerbChanged) IS
	// new information the row should carry, even though its STATUS is not.
	// skipTaskUpdate still means "do not call UpdateTask(newTask), a full
	// blanket rewrite from a stale-snapshot-derived struct" — it does not
	// mean "this Tx writes nothing at all": the status-free single-column
	// tx.TouchTaskUpdatedAt statement these actions may additionally issue
	// carries no status, so it cannot stomp a concurrent transition the way
	// UpdateTask(newTask) would.
	skipTaskUpdate := req.Type == "attrs_set" || req.Type == "child_added" || req.Type == "child_specced" ||
		req.Type == "child_dropped" || req.Type == "noted"

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		// Reopening a terminal execution task whose parent is a card must
		// not exceed the card's own single-work-slot invariant — this child
		// is currently terminal (so it does not itself count toward the
		// slot yet), but a DIFFERENT sibling might already occupy it.
		if req.Type == "reopen" && newTask.Type == orchestrator.TaskTypeExecution && newTask.ParentID != "" {
			parent, perr := tx.GetTask(newTask.ParentID)
			if perr == nil && parent != nil && parent.Type == orchestrator.TaskTypeCard {
				occupied, oerr := cardSlotOccupied(tx, parent.ID)
				if oerr != nil {
					return fmt.Errorf("reopen: check card work slot: %w", oerr)
				}
				if occupied {
					return &StatusError{
						Code:    http.StatusConflict,
						Message: fmt.Sprintf("reopen: card %q's single work slot is already occupied by another child", parent.ID),
					}
				}
			}
		}
		if skipTaskUpdate {
			// Re-validate against a FRESH in-Tx read rather than the pre-Tx
			// snapshot `task`/`newTask` were built from: without this, a
			// concurrent REAL transition (e.g. working→parked, or drop)
			// committed between the pre-Tx read and this Tx opening would
			// leave the side-effect (task_triage.detail fold) correctly
			// applied against fresh state, while the action's own
			// FromStatus/ToStatus audit fields, the Hub broadcast, and the
			// ActionApplication returned to the caller would all still
			// report the STALE pre-race status — and worse, an action that
			// is no longer legal from the task's actual current status
			// would still be recorded as if it succeeded cleanly. Mirrors
			// Wake's own "resolve everything from inside the same
			// transaction" pattern (workflow_card.go).
			fresh, ferr := tx.GetTask(taskID)
			if ferr != nil {
				return statusErrorForGetTaskErr(ferr)
			}
			// Re-validate through the SAME done-status guard, now against
			// the fresh in-Tx read and tx.GetTaskTriage — mirrors the pre-Tx
			// call above one-for-one.
			freshApplied, statusErr := resolveAttrsSetDoneTransition(sm, fresh, action, tx.GetTaskTriage)
			if statusErr != nil {
				return statusErr
			}
			action.FromStatus = fresh.Status
			action.ToStatus = freshApplied.Status
			newTask = freshApplied
		} else {
			if err := tx.UpdateTask(newTask); err != nil {
				return err
			}
		}
		if err := tx.CreateAction(ctx, action); err != nil {
			return err
		}
		// A direct human card transition landing here (working/park/drop/
		// done/reopen — "go" bypasses this whole function via acceptGo
		// above) must not silently discard a still-pending suggestion of a
		// DIFFERENT verb. Guarded by sm.Name, not IsCardTransitionAction
		// alone, since "done"/"reopen" are also execution-machine verb
		// names and IsCardTransitionAction's backing set is keyed on name
		// only.
		if sm.Name == orchestrator.CardMachineName && orchestrator.IsCardTransitionAction(req.Type) {
			if err := recordAndStripSuggestionIfPresent(ctx, tx, newTask.ID, action); err != nil {
				return err
			}
		}
		switch req.Type {
		case "park":
			return applyParkSideEffect(tx, newTask.ID, parkPayloadParsed)
		case "attrs_set":
			var saErr error
			suggestionVerbChanged, saErr = applyAttrsSetSideEffect(tx, newTask.ID, attrsSetParsed)
			if saErr != nil {
				return saErr
			}
			// Bump updated_at, in the SAME Tx, exactly when a suggestion
			// actually ATTACHES — suggestionVerbChanged (the verb differs
			// from what task_triage held before this write) AND the new
			// verb is non-empty (a null-clear/withdrawal must not bump —
			// notifySuggestionArrived below applies the same polarity via
			// this same gate, queue_notify.go). This is deliberately
			// narrower than "the suggestion key was present at all": a
			// judge cycle that resends the IDENTICAL verb unconditionally
			// must not reorder a future updated_at-sorted list on pure
			// machine bookkeeping.
			if suggestionVerbChanged && attrsSetParsed.Verb != "" {
				if err := tx.TouchTaskUpdatedAt(newTask.ID); err != nil {
					return err
				}
			}
			return nil
		case "child_added":
			return applyChildAddedSideEffect(tx, newTask.ID, childAddedParsed)
		case "child_specced":
			return applyChildSpeccedSideEffect(tx, newTask.ID, childSpeccedParsed)
		case "child_dropped":
			return applyChildDroppedSideEffect(tx, newTask.ID, childDroppedParsed)
		case "drop":
			// drop releases every identity bound to this task, atomically
			// with the drop transition itself — so a caller that observes
			// the drop commit can immediately re-link the freed keys to a
			// fresh task. done deliberately has NO case here: done holds
			// identities. machine.go stays a pure transition table (zero
			// side effects); this lives in the service layer alongside
			// park/attrs_set/child_added/child_specced's own side effects.
			return tx.UnlinkAllForTask(newTask.ID)
		}
		return nil
	}); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			return nil, statusErr
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	if s.Hub != nil {
		s.Hub.Broadcast(newTask.ID, TaskEvent{
			Kind: "action",
			Payload: map[string]any{
				"action_id":  action.ID,
				"new_status": string(action.ToStatus),
			},
		})
	}

	// Log every attrs_set that just landed on a done triage task via
	// resolveAttrsSetDoneTransition's guard above — regardless of whether it
	// flips source_closed (SweepReopen decides that separately, on its own
	// tick). See logAttrsSetOnDoneTriage's own doc comment (attrs_set_done.go)
	// for why "log" was chosen over a queue surface. action.FromStatus/
	// newTask.Status are BOTH "done" here precisely when
	// resolveAttrsSetDoneTransition's guard (not the ordinary
	// preExecutionStatuses path) is what let this land.
	//
	// action.FromStatus, NOT the pre-Tx local `fromStatus`: for attrs_set
	// (skipTaskUpdate==true), the in-Tx re-validation above overwrites
	// action.FromStatus with the FRESH in-Tx read, which can legitimately
	// differ from the pre-Tx snapshot's `fromStatus` when a concurrent
	// triage_done commits in the gap between the pre-Tx read and this Tx
	// opening. Keying this check on the stale pre-Tx `fromStatus` would
	// silently skip the log exactly when the guard is what actually let the
	// write through.
	if req.Type == "attrs_set" && action.FromStatus == orchestrator.TaskStatusDone && newTask.Status == orchestrator.TaskStatusDone {
		logAttrsSetOnDoneTriage(s.TaskTriage, newTask.ID)
	}

	// A suggestion attaching (attrs_set with a changed, non-empty verb) is
	// the sole notify trigger for the queue's decision surface — see
	// notifySuggestionArrived's own doc comment (queue_notify.go).
	// suggestionVerbChanged gates out a resend of the SAME verb (which a
	// judge cycle sends unconditionally) so nothing re-notifies when
	// nothing has actually changed.
	if req.Type == "attrs_set" && suggestionVerbChanged {
		s.notifySuggestionArrived(ctx, newTask, attrsSetParsed)
	}

	// newTask.Exec != nil is required here, not merely defensive: a card
	// manual action (park/drop/done/reopen/attrs_set/child_*/noted — every
	// rule except go/answered, which are redirected to acceptGo/applyAnswered
	// earlier in this function) reaches this point with newTask.Exec == nil
	// by construction (a CardAttrs-only row). Coordinator.DispatchAndAdvance
	// unconditionally reads task.Exec.Payload, and this launch runs in an
	// unrecovered goroutine after the HTTP response has already been sent —
	// an unguarded launch here would nil-panic the whole daemon process on
	// the first non-go/answered card action. Mirrors the hook-preview block
	// just below, which already carries this guard.
	if s.Coordinator != nil && newTask.Exec != nil {
		dispatchCtx := s.dispatchCtx
		if dispatchCtx == nil {
			dispatchCtx = context.Background()
		}
		s.dispatchWG.Add(1)
		go func() {
			defer s.dispatchWG.Done()
			s.runDispatchLoop(dispatchCtx, newTask, meta, sm)
		}()
	}

	var matchedHooks []string
	// Behavior is execution-only; Evaluator.Evaluate also only ever matches
	// anything while status == executing (its own gate), so this whole
	// preview is naturally a no-op for a card even without the nil check —
	// the check just avoids the LookupBehavior call on a nil Exec outright.
	if s.Coordinator != nil && newTask.Exec != nil {
		if coord, ok := s.Coordinator.(*orchestrator.Coordinator); ok && coord.Evaluator != nil {
			if behavior, found := orchestrator.LookupBehavior(meta, newTask.Exec.Behavior); found {
				for _, hook := range coord.Evaluator.Evaluate(newTask, behavior.Hooks) {
					matchedHooks = append(matchedHooks, hook.ID)
				}
			}
		}
	}

	return &ActionApplication{
		Task:         newTask,
		Action:       action,
		MatchedHooks: matchedHooks,
	}, nil
}

// runDispatchLoop drives the coordinator through consecutive hook fires until
// the task reaches a terminal or awaiting status, or the task stalls (no
// transition this cycle). Each job clones the project fresh inside the
// sandbox rather than sharing a host git worktree, so concurrent same-branch
// pushes are resolved the ordinary git way (a non-fast-forward reject, then
// pull-and-retry) rather than by serializing same-branch tasks.
func (s *TaskWorkflowService) runDispatchLoop(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) {
	const maxCycles = 10
	current := task

	for cycle := 0; cycle < maxCycles; cycle++ {
		result, err := s.Coordinator.DispatchAndAdvance(ctx, current, meta, sm)
		if err != nil {
			// Persist any partial FiredEvents first so the failing hook
			// remains visible in the timeline; abortOnDispatchError then logs
			// the dispatcher-level error and transitions the task to aborted.
			if result != nil {
				s.persistFiredEvents(ctx, current.ID, current.Status, result.FiredEvents)
			}
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			s.abortOnDispatchError(ctx, current, err)
			return
		}

		s.persistFiredEvents(ctx, current.ID, current.Status, result.FiredEvents)

		// The awaiting trait is owned exclusively by ApplyAction("ask"/"answer")
		// and is persisted to the DB inline as those actions run. Strip it
		// defensively from whatever we're about to merge in case a hook's own
		// patch happens to touch it, so it can never race a concurrent
		// ApplyAction("ask") — pending_answer clearing on the DB-fresh row
		// (below) is the actual source of truth here.
		//
		// Persist ONLY the delta this cycle's hooks actually wrote
		// (result.PayloadDelta), never result.FinalPayload — see
		// DispatchResult.PayloadDelta's doc comment. FinalPayload is built
		// from a snapshot of task.Payload taken BEFORE the hook executed, so
		// applying it wholesale on top of a freshly re-read row would revert
		// any out-of-band write the hook itself made mid-flight (e.g. via
		// `boid task update --payload-patch`) back to its pre-dispatch
		// value. An empty delta ("{}", the common case for an agent job
		// reporting exclusively through the direct RPC paths) is a safe
		// no-op: MergePayload's own empty-update short-circuit returns the
		// fresh row's payload unchanged.
		result.PayloadDelta = orchestrator.StripAwaitingTrait(result.PayloadDelta)

		// Always refresh the task row so we can detect concurrent terminal
		// transitions (abort/done) and pick up any awaiting trait written by
		// an ApplyAction("ask") that fired during the hook.
		var persisted *orchestrator.Task
		if err := s.Tx.WithinTx(func(tx TxStore) error {
			latest, err := tx.GetTask(current.ID)
			if err != nil {
				return err
			}
			// runDispatchLoop only ever drives an execution task (it is the
			// hook-firing loop; a card never dispatches a hook), so
			// latest.Exec is expected non-nil here.
			if latest.Exec == nil {
				return fmt.Errorf("dispatch loop: task %q is not an execution task (no Exec attrs)", latest.ID)
			}
			// Clear pending_answer from the (DB-fresh) awaiting trait now that
			// the hook has been spawned and consumed it. session_id, question,
			// and question_id are preserved so the task can be resumed again
			// if the kit emits another ask.
			latest.Exec.Payload = orchestrator.ClearPendingAnswer(latest.Exec.Payload)
			if len(result.PayloadDelta) > 0 {
				merged, mergeErr := orchestrator.MergePayload(latest.Exec.Payload, result.PayloadDelta)
				if mergeErr != nil {
					return mergeErr
				}
				latest.Exec.Payload = merged
			}
			if err := tx.UpdateTask(latest); err != nil {
				return err
			}
			persisted = latest
			return nil
		}); err != nil {
			slog.Error("persist payload failed", "task_id", current.ID, "error", err)
			s.abortOnDispatchError(ctx, current, fmt.Errorf("persist payload: %w", err))
			return
		}
		current = persisted

		// Drop any would-be auto-advance if the task was terminated
		// concurrently (e.g. user abort while a hook was in flight). Finalize
		// here so the caller that set the terminal status does not have to
		// race with us on cleanup.
		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			slog.Info("dispatch loop: task reached terminal concurrently, skipping advance",
				"task_id", current.ID, "status", current.Status, "would_advance_to", result.NewStatus)
			s.finalizeTerminal(ctx, current)
			return
		}

		// If a hook called boid task notify --ask during this cycle, the task
		// transitioned to awaiting. The lifecycle.executed signal computed from
		// the hook exit is stale — do not auto-advance to done. The dispatch
		// loop will re-fire (via AnswerTask → ApplyAction("answer")) once the
		// user replies.
		if current.Status == orchestrator.TaskStatusAwaiting {
			slog.Info("dispatch loop: task is awaiting user answer, skipping auto-advance",
				"task_id", current.ID, "would_advance_to", result.NewStatus)
			return
		}

		if result.NewStatus == "" {
			// No transition this cycle. Finalize if terminal.
			s.finalizeTerminal(ctx, current)
			return
		}

		prevStatus := current.Status
		action := &orchestrator.Action{
			TaskID:     current.ID,
			Type:       "auto_advance",
			FromStatus: prevStatus,
			ToStatus:   result.NewStatus,
			Payload:    result.ActionPayload,
			Actor:      orchestrator.ActorDaemon,
		}
		current.Status = result.NewStatus
		if err := s.Tx.WithinTx(func(tx TxStore) error {
			if err := tx.UpdateTask(current); err != nil {
				return err
			}
			return tx.CreateAction(ctx, action)
		}); err != nil {
			slog.Error("auto-advance persist failed", "task_id", current.ID, "error", err)
			return
		}

		slog.Info("auto-advanced", "task_id", current.ID, "new_status", current.Status, "cycle", cycle)

		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			s.finalizeTerminal(ctx, current)
			return
		}
	}

	slog.Warn("dispatch loop max cycles reached", "task_id", current.ID, "max", maxCycles)
}

// recordDispatchError persists a dispatch_error action for taskID's audit
// trail. orphanedChildTaskIDs is optional: acceptGo (workflow_card.go) does
// not compensate a failed parked→working commit by aborting children it
// created, since CreateTask's own (ref, parent_id) get-or-create dedup means
// "a child THIS call created" is not actually guaranteed — a concurrent
// second accept(go) racing the same card can observe the SAME already-running
// child, and if ITS OWN transition Tx loses the race, aborting that child
// would kill work a DIFFERENT caller's Tx already committed successfully.
// Leaving the children running and recording their ids here for a human to
// inspect trades a rare "lost a child to a genuinely-failed transition" gap
// for avoiding a categorically worse one.
func (s *TaskWorkflowService) recordDispatchError(ctx context.Context, taskID string, taskStatus orchestrator.TaskStatus, err error, orphanedChildTaskIDs ...string) {
	if s.Tx == nil || taskID == "" || err == nil {
		return
	}

	payloadFields := map[string]any{"error": err.Error()}
	if len(orphanedChildTaskIDs) > 0 {
		payloadFields["orphaned_child_task_ids"] = orphanedChildTaskIDs
	}
	payload, marshalErr := json.Marshal(payloadFields)
	if marshalErr != nil {
		slog.Error("marshal dispatch error payload failed", "task_id", taskID, "error", marshalErr)
		return
	}

	// dispatch_error は状態遷移を伴わないため from_status = to_status = 現在のステータス
	action := &orchestrator.Action{
		TaskID:     taskID,
		Type:       "dispatch_error",
		Payload:    payload,
		FromStatus: taskStatus,
		ToStatus:   taskStatus,
		Actor:      orchestrator.ActorDaemon,
	}
	if txErr := s.Tx.WithinTx(func(tx TxStore) error {
		return tx.CreateAction(ctx, action)
	}); txErr != nil {
		slog.Error("persist dispatch error failed", "task_id", taskID, "error", txErr)
	}
}

// abortOnDispatchError records a dispatch_error action for the audit trail and
// then transitions the task to aborted so terminal cleanup (lifecycle window)
// runs.
//
// When the dispatch context has been canceled (typically because the daemon
// is shutting down via SIGTERM), the abort is recorded with
// code=daemon_shutdown instead of dispatch_error. The startup auto-reopen
// path looks for this code via the derived lifecycle.abort trait and
// re-dispatches the task on next boot. No dispatch_error action is emitted
// for shutdown — that channel is reserved for genuine hook failures.
func (s *TaskWorkflowService) abortOnDispatchError(ctx context.Context, task *orchestrator.Task, err error) {
	shutdown := isShutdownErr(ctx, err)

	code := "dispatch_error"
	message := err.Error()
	if shutdown {
		code = "daemon_shutdown"
		message = "daemon が停止したため中断されました。 起動時に自動 reopen されます。"
	} else {
		s.recordDispatchError(ctx, task.ID, task.Status, err)
	}

	abortPayload, _ := json.Marshal(map[string]string{
		"code":    code,
		"message": message,
	})
	abortAction := &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "abort",
		FromStatus: task.Status,
		ToStatus:   orchestrator.TaskStatusAborted,
		Payload:    abortPayload,
		Actor:      orchestrator.ActorDaemon,
	}
	task.Status = orchestrator.TaskStatusAborted
	if txErr := s.Tx.WithinTx(func(tx TxStore) error {
		if updErr := tx.UpdateTask(task); updErr != nil {
			return updErr
		}
		return tx.CreateAction(ctx, abortAction)
	}); txErr != nil {
		slog.Error("abort on dispatch error: persist abort failed",
			"task_id", task.ID, "error", txErr)
	}
	s.finalizeTerminal(ctx, task)
}

func (s *TaskWorkflowService) persistFiredEvents(ctx context.Context, taskID string, status orchestrator.TaskStatus, events []orchestrator.FiredEvent) {
	if len(events) == 0 || s.Tx == nil {
		return
	}
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		for _, fe := range events {
			payload, _ := json.Marshal(map[string]any{
				"kit_id":       fe.KitID,
				"hook_id":      fe.HandlerID,
				"job_id":       fe.JobID,
				"source_state": fe.SourceState,
				"success":      fe.Success,
				"error":        fe.Error,
			})
			action := &orchestrator.Action{
				TaskID:     taskID,
				Type:       fe.Kind + "_fired",
				Payload:    payload,
				FromStatus: status,
				ToStatus:   status,
				Actor:      orchestrator.ActorDaemon,
			}
			if err := tx.CreateAction(ctx, action); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		slog.Warn("persist fired events failed", "task_id", taskID, "error", err)
		return
	}

	if s.Hub != nil {
		for _, fe := range events {
			s.Hub.Broadcast(taskID, TaskEvent{
				Kind: "fired_event",
				Payload: map[string]any{
					"event_name": fe.Kind + "_fired",
					"role":       fe.HandlerID,
					"kit_id":     fe.KitID,
					"success":    fe.Success,
				},
			})
		}
	}
}

// finalizeTerminal runs the per-task cleanup required once a task has reached
// a terminal status. No-op for non-terminal tasks. Safe to call multiple
// times: CleanupTaskWindow atomically drains runtimes. Every project-visible
// job clones fresh inside the sandbox, so no host worktree or host-local
// branch is ever created for a task's dispatch — there is nothing left on
// the host repo for a terminal task to clean up.
func (s *TaskWorkflowService) finalizeTerminal(ctx context.Context, task *orchestrator.Task) {
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return
	}
	if s.Lifecycle != nil {
		s.Lifecycle.CleanupTaskWindow(task.ID)
	}
	// A terminal task that is a dispatched triage child self-records
	// child_closed on its parent here — see recordChildClosedOnParent's own
	// doc comment (workflow_card.go) for why finalizeTerminal is the right
	// funnel.
	s.recordChildClosedOnParent(task)
}

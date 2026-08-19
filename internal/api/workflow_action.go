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

func (s *TaskWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	// Only Manual:true rules are reachable through this public entry point
	// (HTTP API / brokered action_send / `boid action send` CLI all funnel
	// through ApplyAction). Manual:false rules — job_failed (event-driven,
	// applied directly by CompleteJob), progress/done_request/fail_request
	// (non-transitioning records NotifyTask writes directly), and
	// wake_triaged/wake_ready (resolved internally by Wake via ParkedFrom) —
	// must never be settable by an external caller.
	//
	// This used to be a hand-maintained blocklist naming wake_triaged/
	// wake_ready specifically; codex review round 2 found it had missed
	// job_failed, which (now that pre-execution statuses exist) opened
	// triaged →(job_failed)→aborted →(reopen)→executing as a way to bypass
	// the ready-gate (決定9/逆輸入2) entirely without ever calling "ready" or
	// Go. Checking the machine's own Manual flag instead of a separate list
	// means a future non-manual rule can't be forgotten the same way.
	sm := orchestrator.DefaultMachine()
	if !sm.IsManualAction(req.Type) {
		return nil, &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("action %q is not available through this endpoint", req.Type),
		}
	}

	// docs/plans/ingestion-identity.md PR-2 (B-2), J-10/A-5: action payload
	// size cap. This is action_send's single implementation — HTTP API, Web
	// UI, brokered action_send (boid_executor.go's BoidOpActionSend), and
	// `boid action send` all funnel through ApplyAction, so checking here
	// once covers all of them (one of the 4 mandatory entry points; see
	// orchestrator.ValidateContentSize's own doc comment).
	if err := orchestrator.ValidateContentSize("action payload", req.Payload); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// 決定17 (PR-5b): route `reopen` on a done/aborted TRIAGE task to the
	// triage variant (→ triaged) instead of the ordinary one (→ executing).
	// Done here, right after the task load and before anything downstream
	// keys off req.Type, so every caller — HTTP, Web UI button, brokered
	// task.reopen, CLI — gets it with no wiring of its own. See
	// resolveReopenVariant's doc comment.
	resolvedType, resolveErr := s.resolveReopenVariant(task, req.Type)
	if resolveErr != nil {
		return nil, resolveErr
	}
	req.Type = resolvedType

	// Hydrate with workspace.yaml so kit-supplied hooks / env / capabilities
	// — and, since docs/plans/workspace-default-project.md PR4, the
	// workspace's default project definition (task_behaviors / base_branch /
	// fork_point / default_task_behavior) — are visible to the dispatch
	// loop.
	//
	// On hydration failure the two registration shapes diverge (論点e, PR6):
	//
	//   - a project.yaml-less project (task.ProjectID carries
	//     orchestrator.URLDerivedProjectIDPrefix, PR5) has NO task_behaviors
	//     of its own — every one of them comes from the workspace default
	//     merge this hydrate step performs. Falling back to the bare cached
	//     meta here would silently proceed with a meta that has ZERO
	//     behaviors defined, producing a dispatch failure with no visible
	//     connection to "workspace hydration failed". That is confusing
	//     enough (and the degraded state is real enough — this project
	//     genuinely cannot dispatch anything right now) that it is treated
	//     as a hard error instead of a silent degrade.
	//   - an ordinary project.yaml-bearing project only loses the
	//     workspace-supplied extras (kit env / host_commands / the
	//     workspace-default merge, if any) — its own task_behaviors are
	//     unaffected, so the existing silent-fallback-to-bare-Get behavior
	//     is kept, upgraded with a logged warning (there was previously no
	//     log line here at all) so there is at least a paper trail when
	//     debugging a dispatch that looks like it's missing kit-level
	//     config.
	// isNoYAMLProjectWithNoBehaviors additionally catches the "hydration
	// technically succeeded but degraded" window (Codex review round 1
	// Major): GetWithWorkspace returns (meta, nil error) — success — when the
	// project has no linked workspace, the workspace store is not
	// configured, or workspace.yaml is simply absent (os.ErrNotExist) — see
	// GetWithWorkspace's own doc comment. None of those merge the workspace
	// default in, so a no-YAML project's meta comes back with the same ZERO
	// task_behaviors its bare cached meta always has (synthesizeNoYAMLProjectMeta
	// only ever populates ID/Name). A (meta, nil) check alone would have let
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
	// docs/plans/ingestion-identity.md PR-5 (B-6), I-5b: sm.Apply alone has no
	// rule admitting attrs_set against a done task — this pre-Tx call routes
	// through resolveAttrsSetDoneTransition's service-layer guard first (see
	// its own doc comment, attrs_set_done.go), which falls straight through
	// to sm.Apply for every action/status combination this PR does not touch.
	var getTriage func(string) (*orchestrator.TaskTriage, error)
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
	//
	// reopen_triaged is included (Opus review, Medium): 決定17's routing rewrote
	// req.Type BEFORE this point, so keying on "reopen" alone would drop the
	// message a caller passed to `boid task reopen <card> -m "..."` (or the Web
	// UI reopen dialog) AND — because reopen_triaged is not in
	// sideEffectConsumesPayload either — fall through to MergePayload, polluting
	// task.Payload with the raw {"instruction":...} object. That is precisely the
	// pollution PR-4 called a real regression (see the comment below).
	var reopenPayloadConsumed bool
	if (req.Type == "reopen" || req.Type == "reopen_triaged") && len(req.Payload) > 0 {
		var p struct {
			Instruction *orchestrator.Instruction `json:"instruction,omitempty"`
		}
		if err := json.Unmarshal(req.Payload, &p); err == nil && p.Instruction != nil {
			inst := *p.Instruction
			if active := task.Instructions.Active(); active != nil {
				if inst.Agent == "" {
					inst.Agent = active.Agent
				}
				if inst.Model == "" {
					inst.Model = active.Model
				}
			}
			newTask.Instructions = orchestrator.AppendInstruction(task.Instructions, inst)
			reopenPayloadConsumed = true
		}
	}

	// park / attrs_set / child_added / child_specced all feed their payload
	// exclusively into a task_triage.detail side-effect (below) and the
	// actions table's own audit-trail row — never into task.Payload. This is
	// the same "consumed" treatment reopen already gets, extended per Phase 1
	// PR-4 (docs/plans/cross-project-issue-triage.md 論点6-2): a large
	// attrs_set payload polluting task.Payload (which the CLI/Web UI surface
	// wholesale) would be a real regression, and park had the exact same bug
	// pre-PR-4 (its wake_at/wake_task_id payload was ALSO being merged into
	// task.Payload alongside the task_triage upsert — confirmed while
	// implementing this PR, per 論点6-2's "ついでに確認する" instruction) even
	// though nothing downstream reads it from there.
	//
	// noted/answered (docs/plans/ingestion-identity.md PR-3) join this set
	// for the SAME reason even though neither has a task_triage side effect
	// that "consumes" the payload the way attrs_set's fold does: noted's
	// payload is an opaque workspace-defined blob with no relationship to
	// task.Payload at all, and answered's ({answer,verb,basis}) belongs in
	// the action log + the suggestion-drop side effect only. Merging either
	// into task.Payload would be exactly the pollution bug this map already
	// exists to prevent for the other four.
	sideEffectConsumesPayload := map[string]bool{
		"park":          true,
		"attrs_set":     true,
		"child_added":   true,
		"child_specced": true,
		"noted":         true,
		"answered":      true,
	}

	if !reopenPayloadConsumed && !sideEffectConsumesPayload[req.Type] {
		merged, err := orchestrator.MergePayload(task.Payload, action.Payload)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "payload merge: " + err.Error()}
		}
		newTask.Payload = merged
	}

	// park / attrs_set / child_added / child_specced each validate their
	// payload BEFORE the transaction opens, so a malformed payload surfaces
	// as 400 rather than being swallowed into WithinTx's generic 500 wrapping
	// (park's established pattern, extended per 論点6-4 to the three new
	// triage-vocabulary actions — docs/plans/cross-project-issue-triage.md
	// PR-4 設計メモ 論点4/6).
	var parkPayloadParsed *parkPayload
	var attrsSetParsed *attrsSetPatch
	var childAddedParsed *childAddedPayload
	var childSpeccedParsed *childSpeccedPayload
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
	case "noted":
		// docs/plans/ingestion-identity.md PR-3 (J-5): the daemon never
		// interprets noted's payload — parseNotedPayload only confirms it is
		// syntactically valid JSON (see its own doc comment).
		if perr := parseNotedPayload(req.Payload); perr != nil {
			return nil, perr
		}
	case "answered":
		// The parsed value isn't needed downstream — applyAnsweredSideEffect
		// (below) strips detail.attrs.suggestion unconditionally regardless
		// of answer/verb/basis (J-7: the daemon does not act on their
		// content). parseAnsweredPayload is still called here so a malformed
		// payload surfaces as 400 before the transaction opens, matching
		// every other pre-Tx validation above.
		if _, perr := parseAnsweredPayload(req.Payload); perr != nil {
			return nil, perr
		}
	}

	// attrs_set/child_added/child_specced are non-transitioning (ToStatus==""
	// in the matched rule, machine.go) AND their payload is fully consumed by
	// the side-effect above (never merged into task.Payload either) — so the
	// task row genuinely has nothing new to persist. Skipping tx.UpdateTask
	// for exactly these three closes a race codex review flagged (Major): a
	// caller read `task` before opening this Tx, so `newTask` is built from
	// that stale snapshot; if a REAL transition (e.g. working→parked)
	// committed concurrently in between, an unconditional UpdateTask(newTask)
	// here would silently stomp the fresh status back to the stale one the
	// non-transitioning action happened to read, even though its own
	// side-effect (the sidecar RMW) correctly read-modified-wrote inside this
	// same Tx. park is NOT in this set — it genuinely transitions status, so
	// it still needs the write.
	//
	// noted/answered (docs/plans/ingestion-identity.md PR-3) join this set
	// for the identical reason: both are non-transitioning and their payload
	// is fully consumed (never merged into task.Payload, per
	// sideEffectConsumesPayload above) — an unconditional UpdateTask(newTask)
	// here would risk the same stale-status-stomp race for them too.
	skipTaskUpdate := req.Type == "attrs_set" || req.Type == "child_added" || req.Type == "child_specced" ||
		req.Type == "noted" || req.Type == "answered"

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if skipTaskUpdate {
			// Re-validate against a FRESH in-Tx read rather than the
			// pre-Tx snapshot `task`/`newTask` were built from (codex
			// review round 2 Major fix): without this, a concurrent REAL
			// transition (e.g. working→parked, or drop) committed between
			// the pre-Tx read and this Tx opening would leave the
			// side-effect (task_triage.detail fold) correctly applied
			// against fresh state, while the action's own FromStatus/
			// ToStatus audit fields, the Hub broadcast, and the
			// ActionApplication returned to the caller would all still
			// report the STALE pre-race status — and worse, an action that
			// is no longer legal from the task's actual current status
			// (e.g. attrs_set after a concurrent drop moved it to
			// "dropped", which is not in attrs_set's FromStatus
			// enumeration) would still be recorded as if it succeeded
			// cleanly. Mirrors Wake's own "resolve everything from inside
			// the same transaction" pattern (workflow_triage.go).
			fresh, ferr := tx.GetTask(taskID)
			if ferr != nil {
				return statusErrorForGetTaskErr(ferr)
			}
			// docs/plans/ingestion-identity.md PR-5 (B-6), I-5b: re-validate
			// through the SAME done-status guard, now against the fresh in-Tx
			// read and tx.GetTaskTriage — mirrors the pre-Tx call above one-for-
			// one (same helper, same signature; tx.GetTaskTriage's method value
			// satisfies getTriage's func type directly).
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
		if err := tx.CreateAction(action); err != nil {
			return err
		}
		switch req.Type {
		case "park":
			return applyParkSideEffect(tx, newTask.ID, parkPayloadParsed)
		case "attrs_set":
			return applyAttrsSetSideEffect(tx, newTask.ID, attrsSetParsed)
		case "child_added":
			return applyChildAddedSideEffect(tx, newTask.ID, childAddedParsed)
		case "child_specced":
			return applyChildSpeccedSideEffect(tx, newTask.ID, childSpeccedParsed)
		case "answered":
			// docs/plans/ingestion-identity.md PR-3 (J-6): drops
			// detail.attrs.suggestion — see applyAnsweredSideEffect's own doc
			// comment (workflow_triage.go) for why this runs unconditionally
			// for both accept and reject. "noted" has NO case here (falls
			// through to the default `return nil` below): its payload lives
			// only in the action row itself (J-5, daemon never interprets it).
			return applyAnsweredSideEffect(tx, newTask.ID)
		case "drop":
			// docs/plans/ingestion-identity.md PR-1 (B-1), I-6: drop releases
			// every identity bound to this task, atomically with the drop
			// transition itself — so a caller that observes the drop commit
			// can immediately re-link the freed keys to a fresh task. done
			// (I-5) deliberately has NO case here: done holds identities.
			// machine.go stays a pure transition table (zero side effects);
			// this lives in the service layer alongside park/attrs_set/
			// child_added/child_specced's own side effects.
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

	// docs/plans/ingestion-identity.md PR-5 (B-6), I-5c: log every attrs_set
	// that just landed on a done triage task via I-5b's guard above —
	// regardless of whether it flips source_closed (SweepReopen decides that
	// separately, on its own tick). See logAttrsSetOnDoneTriage's own doc
	// comment (attrs_set_done.go) for why "log" was chosen over a queue
	// surface. action.FromStatus/newTask.Status are BOTH "done" here
	// precisely when resolveAttrsSetDoneTransition's guard (not the ordinary
	// preExecutionStatuses path) is what let this land.
	//
	// action.FromStatus, NOT the pre-Tx local `fromStatus` (2026-08-19 fix,
	// Opus review N-5): for attrs_set (skipTaskUpdate==true), the in-Tx
	// re-validation above overwrites action.FromStatus with the FRESH
	// in-Tx read (`action.FromStatus = fresh.Status`) — which can legitimately
	// differ from the pre-Tx snapshot's `fromStatus` when a concurrent
	// triage_done commits in the gap between the pre-Tx read and this Tx
	// opening (pre-Tx: triaged/working; in-Tx: done). Keying this check on
	// the STALE pre-Tx `fromStatus` would silently skip the log exactly when
	// I-5b's guard is what actually let the write through — the opposite of
	// "every attrs_set that lands via the guard gets logged".
	if req.Type == "attrs_set" && action.FromStatus == orchestrator.TaskStatusDone && newTask.Status == orchestrator.TaskStatusDone {
		logAttrsSetOnDoneTriage(s.TaskTriage, newTask.ID)
	}

	// Phase 1 PR-2 (docs/plans/cross-project-issue-triage.md, 逆輸入2): "Go 操作
	// = ready 遷移 + 機械 dispatch の 2 段で実装する". The "ready" action above IS
	// Go (nose's judgment, Manual:true); immediately chaining into Dispatch
	// here is the mechanical second stage, requiring no further judgment
	// (決定12) — same idiom as CreateTask's auto_start chaining directly into
	// ApplyAction("start"). A Dispatch failure (e.g. a malformed child spec,
	// or a project that no longer exists) is logged, not surfaced as this
	// call's error: the "ready" transition itself already committed
	// successfully, and per the state machine's own doc comment "working
	// deliberately has NO exit rule yet" a task stuck in ready is the
	// intended, visible failure mode (機構の不調のサイン) rather than a lost
	// Go.
	if req.Type == "ready" && newTask.Status == orchestrator.TaskStatusReady {
		dispatched, dispatchErr := s.Dispatch(ctx, newTask.ID)
		if dispatchErr != nil {
			slog.Error("ready: machine dispatch (ready->working) failed", "task_id", newTask.ID, "error", dispatchErr)
		} else if dispatched != nil {
			newTask = dispatched.Task
		}
	}

	// queue の決定論的評価 節 rule 4 (notify, PR-3 scoped-down slice — see
	// queue_notify.go's doc comment). Checked against newTask AFTER the
	// ready->working auto-dispatch above: if dispatch succeeded, newTask.Status
	// is "working" (not a queue-member status), so no spurious notify fires
	// for a task nose already Go'd through to execution — only a task that
	// genuinely RESTS in triaged/ready notifies.
	s.notifyQueueEntryIfUrgent(ctx, newTask, fromStatus)
	// rule 4's other entry point: urgency arriving on a card that is already a
	// queue member (see notifyUrgencyRaised's doc comment for why the
	// entry-transition detector above cannot see this, and why the order it
	// misses is the natural one).
	if req.Type == "attrs_set" {
		s.notifyUrgencyRaised(ctx, newTask, attrsSetParsed)
	}

	if s.Coordinator != nil {
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
	if s.Coordinator != nil {
		if coord, ok := s.Coordinator.(*orchestrator.Coordinator); ok && coord.Evaluator != nil {
			if behavior, found := orchestrator.LookupBehavior(meta, newTask.Behavior); found {
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
// transition this cycle). Branch-level serialization (the former
// BranchLockManager) was retired in docs/plans/git-gateway-cutover.md PR6:
// each job now clones the project fresh inside the sandbox instead of
// sharing a host git worktree, so the physical constraint that motivated
// serializing same-branch tasks (only one worktree can check out a given
// branch at a time) no longer exists. Concurrent same-branch pushes are
// instead resolved the ordinary git way — the second push hits a
// non-fast-forward reject and that session pulls (fetch + merge/rebase)
// before retrying — which also resolves the branch-lock head-of-line
// blocking a long-lived supervisor could previously cause (see
// khi-supervisor-branch-lock-headline-block in memory).
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
				s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)
			}
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			s.abortOnDispatchError(ctx, current, err)
			return
		}

		s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)

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
		// value — the exact silent-revert-after-CLI-success bug codex review
		// caught (Phase 5b PR7, wiring-seams.md #17's Blocker 1). An empty
		// delta ("{}", the common case for an agent job reporting
		// exclusively through the direct RPC paths) is a safe no-op:
		// MergePayload's own empty-update short-circuit returns the fresh
		// row's payload unchanged, so there is nothing left that could ever
		// overwrite a concurrent write.
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
			// Clear pending_answer from the (DB-fresh) awaiting trait now that
			// the hook has been spawned and consumed it. session_id, question,
			// and question_id are preserved so the task can be resumed again
			// if the kit emits another ask.
			latest.Payload = orchestrator.ClearPendingAnswer(latest.Payload)
			if len(result.PayloadDelta) > 0 {
				merged, mergeErr := orchestrator.MergePayload(latest.Payload, result.PayloadDelta)
				if mergeErr != nil {
					return mergeErr
				}
				latest.Payload = merged
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
			return tx.CreateAction(action)
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

func (s *TaskWorkflowService) recordDispatchError(taskID string, taskStatus orchestrator.TaskStatus, err error) {
	if s.Tx == nil || taskID == "" || err == nil {
		return
	}

	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
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
		return tx.CreateAction(action)
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
		s.recordDispatchError(task.ID, task.Status, err)
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
		return tx.CreateAction(abortAction)
	}); txErr != nil {
		slog.Error("abort on dispatch error: persist abort failed",
			"task_id", task.ID, "error", txErr)
	}
	s.finalizeTerminal(ctx, task)
}

func (s *TaskWorkflowService) persistFiredEvents(taskID string, status orchestrator.TaskStatus, events []orchestrator.FiredEvent) {
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
			if err := tx.CreateAction(action); err != nil {
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
// times: CleanupTaskWindow atomically drains runtimes.
//
// Worktree disk cleanup and boid/<id8> branch sweeping (the former
// cleanupWorktree / sweepChildBranches, backed by dispatcher.WorktreeManager)
// were retired in docs/plans/git-gateway-cutover.md PR8: every project-visible
// job clones fresh inside the sandbox (PR6 cutover), so no host worktree or
// host-local boid/<id8> branch is ever created for a task's dispatch — there
// is nothing left on the host repo for a terminal task to clean up.
func (s *TaskWorkflowService) finalizeTerminal(ctx context.Context, task *orchestrator.Task) {
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return
	}
	if s.Lifecycle != nil {
		s.Lifecycle.CleanupTaskWindow(task.ID)
	}
	// Phase 1 PR-4 (docs/plans/cross-project-issue-triage.md 論点9): a
	// terminal task that is a dispatched triage child self-records
	// child_closed on its parent here — see recordChildClosedOnParent's own
	// doc comment (workflow_triage.go) for why finalizeTerminal is the right
	// funnel.
	s.recordChildClosedOnParent(task)
}

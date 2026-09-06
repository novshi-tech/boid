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

// validateParentlessExecutorBase surfaces the "parent-less executor pointed
// at a non-existent base" error at task creation time, catching a
// user-visible config bug (a top-level executor whose base_branch does not
// exist on origin) before it fails deep inside the sandbox clone.
//
// The function is conservative: when classification itself fails (e.g.
// detached HEAD, project lookup unwired) the error is surfaced so callers
// cannot silently fall through to a broken sandbox run.
//
// Lives on the service (rather than the orchestrator package) because the
// decision combines task-row metadata (behaviorName, parent), project meta
// (workdir lookup), and orchestrator primitives, and orchestrator may not
// import api's ProjectWorkDirLookup interface back.
func (s *TaskAppService) validateParentlessExecutorBase(req CreateTaskRequest, behaviorName, baseBranch string) error {
	if behaviorName != "executor" {
		// Only parent-less executor with a case-3 base is a creation-time
		// error; every other combination is either fine or handled downstream.
		return nil
	}
	if req.ParentID != "" {
		// Child executor inherits its parent's base_branch responsibility.
		return nil
	}
	if s.Projects == nil {
		// No project workdir lookup available (e.g. test wiring without a
		// Projects stub). Without it we cannot classify; skip the check.
		return nil
	}
	proj, projErr := s.Projects.GetProject(req.ProjectID)
	if projErr != nil {
		return &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project lookup failed: %v", projErr)}
	}
	if proj == nil || proj.WorkDir == "" {
		return nil
	}

	state, err := orchestrator.ClassifyBaseBranch(proj.WorkDir, baseBranch)
	if err != nil {
		return &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("classify base_branch %q: %v", baseBranch, err)}
	}
	if state == orchestrator.Case3NotFound {
		return &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("executor base_branch %q does not exist locally or on origin, and the task has no parent supervisor to create it", baseBranch),
		}
	}
	return nil
}

// allowedCreateInitialStatuses is the allowlist for CreateTaskRequest.InitialStatus.
// Deliberately does NOT include every orchestrator.TaskStatus value — a
// caller must not be able to fabricate a task that's already
// "done"/"executing"/etc; only the two entry points a task can legitimately
// start from are allowed. A card is born directly into "parked" — there is
// no "captured"/"triaged" initial status.
var allowedCreateInitialStatuses = map[string]orchestrator.TaskStatus{
	"":        orchestrator.TaskStatusPending, // unchanged default
	"pending": orchestrator.TaskStatusPending,
	"parked":  orchestrator.TaskStatusParked,
}

// resolveInitialStatus validates req.InitialStatus and returns the
// orchestrator.TaskStatus to create the task with (never "" — callers pass
// this straight to orchestrator.Task.Status; store.go's own `if t.Status ==
// ""` fallback to pending is never relied on here, keeping this the single
// place that decides a new task's starting status).
func resolveInitialStatus(req CreateTaskRequest) (orchestrator.TaskStatus, error) {
	status, ok := allowedCreateInitialStatuses[req.InitialStatus]
	if !ok {
		return "", &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("initial_status: unknown value %q (allowed: pending, parked)", req.InitialStatus),
		}
	}
	if req.AutoStart && status != orchestrator.TaskStatusPending {
		return "", &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("auto_start cannot be combined with initial_status %q (task would never reach pending)", req.InitialStatus),
		}
	}
	return status, nil
}

// idempotencyKeyTypeMismatchErr mirrors orchestrator.CreateTask's own
// rejectIdempotencyKeyTypeMismatch guard: the service-layer get-or-create
// short-circuits before ever reaching that store-layer check, so it needs
// its own copy to keep an idempotency-key hit from silently handing back a
// wrong-typed task.
func idempotencyKeyTypeMismatchErr(wantType orchestrator.TaskType, existing *orchestrator.Task, key, projectID, parentID string) error {
	if existing.Type == wantType {
		return nil
	}
	return &StatusError{
		Code: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"idempotency_key %q (project_id=%s, parent_id=%s) already used by a %s task (id=%s); this create requested a %s task",
			key, projectID, parentID, existing.Type, existing.ID, wantType),
	}
}

func (s *TaskAppService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	initialStatus, err := resolveInitialStatus(req)
	if err != nil {
		return nil, err
	}

	if err := orchestrator.ValidateContentSize("description", []byte(req.Description)); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// A card (initial_status=parked) never calls ResolveBehavior at all —
	// Behavior/Traits/Readonly/BranchPrefix/BaseBranch/Payload/Instructions/
	// AutoStart are execution-only fields a card structurally cannot carry
	// (ExecAttrs is nil), so there is nothing for behavior resolution to
	// feed into.
	if initialStatus == orchestrator.TaskStatusParked {
		return s.createCardTask(req, initialStatus)
	}
	return s.createExecutionTask(req, initialStatus)
}

// createCardTask builds and inserts a fresh Card. A card created here starts
// with empty CardAttrs (kind/urgency/wake_at/wake_task_id/suggestion_verb/
// detail all zero-valued); the row is type='card' from the INSERT itself, so
// a card cannot be born rowless and needs no separate seeding step.
func (s *TaskAppService) createCardTask(req CreateTaskRequest, initialStatus orchestrator.TaskStatus) (*orchestrator.Task, error) {
	// Children inherit remote_id from their parent when they don't supply
	// their own (see createExecutionTask's matching comment for the full
	// rationale — remote_id is a common-core field, so this applies to a
	// card exactly the same way).
	if req.RemoteID == "" && req.ParentID != "" {
		if parent, parentErr := s.Tasks.GetTask(req.ParentID); parentErr == nil && parent != nil && parent.RemoteID != "" {
			req.RemoteID = parent.RemoteID
		}
	}

	if req.Ref != "" {
		existing, err := s.Tasks.FindTaskByRef(req.Ref, req.ParentID, req.ProjectID)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		if existing != nil {
			return existing, nil
		}
	}
	if req.Ref == "" && req.IdempotencyKey != "" {
		existing, err := s.Tasks.FindTaskByIdempotencyKey(req.ProjectID, req.ParentID, req.IdempotencyKey)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		if existing != nil {
			if merr := idempotencyKeyTypeMismatchErr(orchestrator.TaskTypeCard, existing, req.IdempotencyKey, req.ProjectID, req.ParentID); merr != nil {
				return nil, merr
			}
			return existing, nil
		}
	}

	// A card-type child has no legitimate "fulfilling a specced reservation"
	// story (acceptGo only ever dispatches execution tasks) — pass empty
	// ref/behavior so cardChildSlotConflict's exception can never match and
	// any real occupant unconditionally blocks this create.
	if req.ParentID != "" {
		if parent, perr := s.Tasks.GetTask(req.ParentID); perr == nil && parent != nil && parent.Type == orchestrator.TaskTypeCard {
			if conflict, occupant := s.cardSlotConflictWithRequests(parent, "", "", "", ""); conflict {
				return nil, &StatusError{
					Code: http.StatusConflict,
					Message: fmt.Sprintf(
						"create task: card %q's single work slot is already occupied by %s",
						req.ParentID, occupant),
				}
			}
		}
	}

	task := &orchestrator.Task{
		ID:             req.ID,
		Type:           orchestrator.TaskTypeCard,
		ProjectID:      req.ProjectID,
		Title:          req.Title,
		Description:    req.Description,
		Status:         initialStatus,
		RemoteID:       req.RemoteID,
		Ref:            req.Ref,
		ParentID:       req.ParentID,
		IdempotencyKey: req.IdempotencyKey,
		Card:           &orchestrator.CardAttrs{},
	}
	if err := s.Tasks.CreateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return task, nil
}

// attachCardRequestIfNeeded runs after a Ref/IdempotencyKey get-or-create
// hit found an EXISTING task: without this, a launcher-supplied ref or
// idempotency_key would return early and skip CardRequestLinker entirely,
// leaving the request permanently "launching" with no continuation ever
// attached. Reusing CreateTaskLinkedToCardRequest on the already-found
// existing task is safe and idempotent — its own CreateTask call resolves
// straight back to the same row.
func (s *TaskAppService) attachCardRequestIfNeeded(existing *orchestrator.Task, cardRequestID string) (*orchestrator.Task, error) {
	if cardRequestID == "" || s.CardRequestLinker == nil {
		return existing, nil
	}
	if err := s.CardRequestLinker.CreateTaskLinkedToCardRequest(existing, cardRequestID); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return existing, nil
}

// cardChildSlotConflict reports whether creating (or reparenting/rerunning)
// an execution task with the given ref/projectID/behavior under parent
// (already confirmed to be type=card) would violate the card's
// single-work-slot invariant: at most one open/specced/dispatched child at
// a time. parent.OpenChildCount covers a live task row (dispatched, or
// created via a bypass with no JSON entry); an open/specced JSON child not
// yet task-ified is the other half.
//
// Fulfilling that child's own reservation (ref == its id, project/behavior
// matching what child_specced recorded on its Spec — matching by ref alone
// is spoofable) is not a NEW occupant and is let through, but ONLY when it
// is the SOLE occupant: a second unresolved sibling still blocks, since
// fulfilling one reservation does not free up room for another. The
// project/behavior match raises the bar, not a hard close — card_read.go
// exposes both to anyone who can already read the card.
//
// A plain read-then-decide check, not wrapped in a transaction — same
// race-tolerant posture as the ref-based get-or-create above (a losing
// concurrent caller gets a clear 409 to retry). occupant is always a
// ready-to-use noun phrase for embedding directly in a 409 message.
func cardChildSlotConflict(parent *orchestrator.Task, ref, projectID, behavior string) (conflict bool, occupant string) {
	if parent.OpenChildCount > 0 {
		return true, "a live child task"
	}
	var detail json.RawMessage
	if parent.Card != nil {
		detail = parent.Card.Detail
	}
	children, err := orchestrator.DetailChildren(detail)
	if err != nil {
		return false, ""
	}
	var occupants []orchestrator.TaskTriageChild
	for _, c := range children {
		if c.Status == orchestrator.TaskTriageChildStatusOpen || c.Status == orchestrator.TaskTriageChildStatusSpecced {
			occupants = append(occupants, c)
		}
	}
	if len(occupants) == 0 {
		return false, ""
	}
	if len(occupants) == 1 {
		c := occupants[0]
		if c.ID == ref && c.Spec != nil && c.Spec.Project == projectID && c.Spec.Behavior == behavior {
			return false, ""
		}
	}
	return true, fmt.Sprintf("child %q", occupants[0].ID)
}

func (s *TaskAppService) createExecutionTask(req CreateTaskRequest, initialStatus orchestrator.TaskStatus) (*orchestrator.Task, error) {
	var meta *orchestrator.ProjectMeta
	if s.Meta != nil {
		// Hydrate with workspace.yaml so a workspace-level default project
		// definition's task_behaviors are visible to ResolveBehavior, not
		// just project.yaml's own. Falls back to bare Get on any hydration
		// failure (same idiom as ProjectAppService.hydrateProjectWithWorkspace
		// and TaskWorkflowService.ApplyAction) — this preserves CreateTask's
		// "meta not loaded → nil meta, continue" tolerance and additionally
		// degrades gracefully, rather than failing task creation outright,
		// for the failure modes GetWithWorkspace can produce that bare Get
		// never could (a corrupt workspace.yaml, a host_commands conflict).
		if hydrated, err := s.Meta.GetWithWorkspace(context.Background(), req.ProjectID); err == nil && hydrated != nil {
			meta = hydrated
		} else if m, ok := s.Meta.Get(req.ProjectID); ok {
			meta = m
		}
	}

	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{
		Behavior:     req.Behavior,
		BehaviorSpec: req.BehaviorSpec,
		Payload:      req.Payload,
		Instructions: req.Instructions,
	})
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	traits := res.Traits
	readonly := res.Readonly
	branchPrefix := res.BranchPrefix
	baseBranch := res.BaseBranch
	payload := res.Payload

	if req.Traits != nil {
		traits = req.Traits
	}
	if req.Readonly != nil {
		readonly = *req.Readonly
	}
	// worktree / base_branch / branch_prefix come from the resolved behavior
	// (and project-level defaults). readonly is a first-class override: when
	// supplied, it wins over the behavior default.

	// Children inherit remote_id from their parent when they don't supply
	// their own. With base_branch derived from the project-top template +
	// remote_id, this keeps "parent and child share the same feature branch"
	// the default without forcing every spawn site to pass remote_id by hand.
	// Explicit remote_id on the child overrides the parent's (cross-track
	// children stay supported). base_branch itself is NOT inherited — each
	// task resolves it from its own project-top template + its own
	// (possibly inherited) remote_id, so cross-project parent/child works
	// correctly without dragging the parent project's branch into the child.
	if req.RemoteID == "" && req.ParentID != "" {
		if parent, parentErr := s.Tasks.GetTask(req.ParentID); parentErr == nil && parent != nil && parent.RemoteID != "" {
			req.RemoteID = parent.RemoteID
		}
	}
	if baseBranch == "" {
		// A task with no base_branch expands ${current_branch}; detached
		// HEAD is surfaced as a 400. Every project-visible dispatch needs a
		// resolvable base_branch to build its sandbox-internal
		// CloneDeclaration, so this applies regardless of behavior name, not
		// just to the canonical supervisor/executor behaviors.
		if s.Projects != nil {
			proj, projErr := s.Projects.GetProject(req.ProjectID)
			if projErr != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project lookup failed: %v", projErr)}
			}
			if proj != nil && proj.WorkDir != "" {
				expanded, err := orchestrator.ExpandBaseBranch("${current_branch}", proj.WorkDir)
				if err != nil {
					return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("base_branch: %v", err)}
				}
				baseBranch = expanded
			}
		}
	} else if baseBranch != "" {
		// Explicit base: expand ${TASK_REMOTE_ID} first so a missing
		// remote_id errors out before we touch the project working
		// directory, then expand ${current_branch}.
		expanded, err := orchestrator.ExpandTaskBaseBranch(baseBranch, req.RemoteID)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		baseBranch = expanded

		if s.Projects != nil {
			proj, projErr := s.Projects.GetProject(req.ProjectID)
			if projErr != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project lookup failed: %v", projErr)}
			}
			expanded, err := orchestrator.ExpandBaseBranch(baseBranch, proj.WorkDir)
			if err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
			}
			baseBranch = expanded
		}
	}

	// Creation-time guard: reject a parent-less executor whose base_branch
	// does not exist on origin.
	if err := s.validateParentlessExecutorBase(req, res.BehaviorName, baseBranch); err != nil {
		return nil, err
	}

	// Get-or-create: when ref is present, check for an existing task (scoped
	// by parent_id, which is "" for a root task) before building and
	// inserting a new one. This is the service-level dedup guard; the store
	// has an identical check for the concurrent-create race. Returning early
	// here avoids a redundant INSERT round-trip.
	//
	// This applies to root tasks too, not just children, which is what makes
	// ingestion push idempotent: a caller creates a task with
	// Ref=<source_ref> (jira issue_key / slack thread_ts / mail message-id),
	// and a resend after a crash returns the SAME existing task instead of a
	// duplicate. The unique index on (ref, parent_id) already covers
	// parent_id="" rows uniquely (parent_id is `NOT NULL DEFAULT ''`, never
	// SQL NULL), treating every root task's ref as unique among root tasks
	// the same way it already does for a given parent's children.
	if req.Ref != "" {
		existing, err := s.Tasks.FindTaskByRef(req.Ref, req.ParentID, req.ProjectID)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		if existing != nil {
			// First-write-wins: return the existing task. Do not fire auto_start
			// because the task may already be executing or terminal.
			return s.attachCardRequestIfNeeded(existing, req.CardRequestID)
		}
	}

	// Get-or-create by IdempotencyKey, independent of Ref (a Ref-miss must
	// not fall through to the card slot check below, which would otherwise
	// see this same idempotency key's already-created live row as a
	// competing occupant). Runs before the card slot check for the same
	// reason. A pending hit with req.AutoStart set is rescued into "start"
	// itself, since a hit here returns before ever reaching the ordinary
	// auto_start block below.
	if req.IdempotencyKey != "" {
		existing, err := s.Tasks.FindTaskByIdempotencyKey(req.ProjectID, req.ParentID, req.IdempotencyKey)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		if existing != nil {
			if merr := idempotencyKeyTypeMismatchErr(orchestrator.TaskTypeExecution, existing, req.IdempotencyKey, req.ProjectID, req.ParentID); merr != nil {
				return nil, merr
			}
			if req.AutoStart && s.Workflow != nil && existing.Status == orchestrator.TaskStatusPending {
				result, err := s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), existing.ID, ApplyActionRequest{Type: "start"})
				if err != nil {
					slog.Error("auto_start: failed to apply start action on idempotency-key hit", "task_id", existing.ID, "error", err)
				} else {
					existing = result.Task
				}
			}
			return s.attachCardRequestIfNeeded(existing, req.CardRequestID)
		}
	}

	// The card's single-work-slot invariant applies to this write port too:
	// any DIRECT task creation under a card (CLI, HTTP API, acceptGo's own
	// CreateTask call) must not exceed one open/specced/dispatched child.
	// See cardChildSlotConflict's own doc comment for why fulfilling the
	// currently-specced child's own reservation (Ref matching its id —
	// acceptGo's convention) is not treated as a new occupant.
	//
	// A CardRequestID-less create takes no card_requests row to arbitrate
	// with, and a CardRequestID-carrying create's own reservation is
	// excluded from the conflict check (it's the claim being fulfilled, not
	// a new occupant) — neither has a lock of its own. When s.Tx is wired,
	// both route through the SAME WithinTx call below (atomicCardCheck): a
	// fresh re-read plus the INSERT happen atomically, closing the
	// read-then-write gap a plain pre-check would otherwise leave open.
	// Only when s.Tx is nil does this fall back to the pre-check below,
	// followed by a separate write.
	var cardParent *orchestrator.Task
	if req.ParentID != "" {
		if parent, perr := s.Tasks.GetTask(req.ParentID); perr == nil && parent != nil && parent.Type == orchestrator.TaskTypeCard {
			cardParent = parent
		}
		// A parent lookup failure here is deliberately non-fatal: this is a
		// defense-in-depth check, not the parent existence check itself (a
		// genuinely missing/unreadable parent surfaces its own error further
		// down the ordinary create path, same posture as the remote_id
		// inheritance lookups above).
	}
	atomicCardCheck := cardParent != nil && s.Tx != nil
	if cardParent != nil && !atomicCardCheck {
		if conflict, occupant := s.cardSlotConflictWithRequests(cardParent, req.Ref, req.ProjectID, req.Behavior, req.CardRequestID); conflict {
			return nil, &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"create task: card %q's single work slot is already occupied by %s",
					req.ParentID, occupant),
			}
		}
	}

	task := &orchestrator.Task{
		ID:             req.ID,
		Type:           orchestrator.TaskTypeExecution,
		ProjectID:      req.ProjectID,
		Title:          req.Title,
		Description:    req.Description,
		Status:         initialStatus,
		RemoteID:       req.RemoteID,
		Ref:            req.Ref,
		ParentID:       req.ParentID,
		IdempotencyKey: req.IdempotencyKey,
		Exec: &orchestrator.ExecAttrs{
			Behavior:     res.BehaviorName,
			Traits:       traits,
			Readonly:     readonly,
			BranchPrefix: branchPrefix,
			BaseBranch:   baseBranch,
			Payload:      payload,
			Instructions: res.Instructions,
			AutoStart:    req.AutoStart,
		},
	}
	// A card-command launcher's own task continuation (BoidOpTaskCreate's
	// ownership-verified CardRequestID) must be persisted atomically with
	// the request→task association — see CardRequestTaskLinker's own doc
	// comment. atomicCardCheck (cardParent != nil && s.Tx != nil, covering
	// both CardRequestID cases) takes priority: the CardRequestLinker branch
	// below now only serves a launcher's own continuation create (ParentID
	// is the launched ROOT task, not the card — cardParent is nil there) or
	// the s.Tx-unwired fallback.
	switch {
	case atomicCardCheck:
		txErr := s.Tx.WithinTx(func(tx TxStore) error {
			freshParent, gerr := tx.GetTask(cardParent.ID)
			if gerr != nil {
				return gerr
			}
			if conflict, occupant := cardSlotConflictWithLister(tx, freshParent, req.Ref, req.ProjectID, req.Behavior, req.CardRequestID); conflict {
				return &StatusError{
					Code: http.StatusConflict,
					Message: fmt.Sprintf(
						"create task: card %q's single work slot is already occupied by %s",
						req.ParentID, occupant),
				}
			}
			if req.CardRequestID != "" {
				return tx.CreateTaskLinkedToCardRequest(task, req.CardRequestID)
			}
			return tx.CreateTask(task)
		})
		if txErr != nil {
			var se *StatusError
			if errors.As(txErr, &se) {
				return nil, se
			}
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: txErr.Error()}
		}
	case req.CardRequestID != "" && s.CardRequestLinker != nil:
		if err := s.CardRequestLinker.CreateTaskLinkedToCardRequest(task, req.CardRequestID); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	default:
		if err := s.Tasks.CreateTask(task); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	}
	// Guard: only fire auto_start for a freshly pending task. Reachable when
	// the store's OWN get-or-create (a concurrent create race that landed
	// between the service-layer Ref/IdempotencyKey checks above and this
	// call) returns an existing task rather than inserting a new row — an
	// existing task that is executing/awaiting/done/aborted never re-fires
	// start either way. A caller's own Ref/IdempotencyKey hit is handled
	// above and never reaches here at all.

	// CreateTask has no ctx parameter, so this always stamps ActorHuman even
	// though this call also backs `boid task create` from inside a sandbox
	// and Dispatch's child-task creation, both of which should really carry
	// the creating task's own actor.
	if req.AutoStart && s.Workflow != nil && task.Status == orchestrator.TaskStatusPending {
		result, err := s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("auto_start: failed to apply start action", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}
	return task, nil
}

func (s *TaskAppService) ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error) {
	result := &ImportResult{Errors: []ImportError{}}
	for i, req := range reqs {
		if req.RemoteID == "" {
			result.Errors = append(result.Errors, ImportError{
				Line:     i + 1,
				RemoteID: req.RemoteID,
				Error:    "remote_id is required",
			})
			continue
		}

		existing, err := s.Tasks.FindTaskByRemote(req.RemoteID)
		if err != nil {
			result.Errors = append(result.Errors, ImportError{Line: i + 1, RemoteID: req.RemoteID, Error: err.Error()})
			continue
		}
		if existing != nil {
			result.Skipped++
			continue
		}

		if _, err := s.CreateTask(req); err != nil {
			msg := err.Error()
			if se, ok := err.(*StatusError); ok {
				msg = se.Message
			}
			result.Errors = append(result.Errors, ImportError{Line: i + 1, RemoteID: req.RemoteID, Error: msg})
			continue
		}
		result.Created++
	}
	return result, nil
}

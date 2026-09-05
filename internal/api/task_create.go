package api

import (
	"context"
	"encoding/json"
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

	// A card-type child has no legitimate "fulfilling a specced reservation"
	// story (acceptGo only ever dispatches execution tasks) — pass empty
	// ref/behavior so cardChildSlotConflict's exception can never match and
	// any real occupant unconditionally blocks this create.
	if req.ParentID != "" {
		if parent, perr := s.Tasks.GetTask(req.ParentID); perr == nil && parent != nil && parent.Type == orchestrator.TaskTypeCard {
			if conflict, occupant := cardChildSlotConflict(parent, "", "", ""); conflict {
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

// cardChildSlotConflict reports whether creating (or reparenting/rerunning)
// an execution task with the given ref/projectID/behavior under parent
// (already confirmed to be type=card) would violate the card's
// single-work-slot invariant: at most one open/specced/dispatched child at
// a time.
//
// parent.OpenChildCount (a live, non-terminal task row under it — the
// column GetTask already populates, independent of task_triage.detail)
// covers the "already dispatched, or created via a bypass with no JSON
// entry at all" half — reported with a generic occupant description since
// there is no cheap way to name the specific live child from a plain Task
// struct alone. The JSON-only half — an open/specced child not yet
// task-ified — is orchestrator.DetailOpenSlotChildID.
//
// Fulfilling that exact child's own reservation (ref == its id, the
// convention acceptGo's own CreateTask call always uses, workflow_card.go)
// is not a NEW occupant and is let through — this is what makes Go's own
// child-ification not trip the very guard it must also respect. Matching by
// ref ALONE is not enough: ref is fully caller-controlled and the occupant
// id is readable from the card's own detail, so a caller could otherwise
// plant an unrelated task (wrong project/behavior) under a matching ref,
// which a later legitimate acceptGo call would then adopt via
// FindTaskByRef's own get-or-create instead of ever creating the actually-
// specced work. Requiring projectID/behavior to match what child_specced
// itself recorded on that child's Spec closes this.
//
// This is a plain read-then-decide check, not wrapped in a transaction —
// the same race-tolerant posture task_create.go's own ref-based
// get-or-create already documents ("service-level dedup guard; the store
// has an identical check for the concurrent-create race"): a genuine
// concurrent double-create is rare for this single-operator-per-card
// feature, and a caller that loses the race gets a clear 409 to retry.
// cardChildSlotConflict's occupant return value is always a ready-to-use
// noun phrase (never a bare id) so every call site can embed it directly in
// a 409 message without needing to know which of the two occupancy sources
// fired.
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
	for _, c := range children {
		if c.Status != orchestrator.TaskTriageChildStatusOpen && c.Status != orchestrator.TaskTriageChildStatusSpecced {
			continue
		}
		if c.ID == ref && c.Spec != nil && c.Spec.Project == projectID && c.Spec.Behavior == behavior {
			return false, ""
		}
		return true, fmt.Sprintf("child %q", c.ID)
	}
	return false, ""
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
			return existing, nil
		}
	}

	// The card's single-work-slot invariant applies to this write port too:
	// any DIRECT task creation under a card (CLI, HTTP API, acceptGo's own
	// CreateTask call) must not exceed one open/specced/dispatched child.
	// See cardChildSlotConflict's own doc comment for why
	// fulfilling the currently-specced child's own reservation (Ref matching
	// its id — acceptGo's convention) is not treated as a new occupant.
	if req.ParentID != "" {
		parent, perr := s.Tasks.GetTask(req.ParentID)
		if perr == nil && parent != nil && parent.Type == orchestrator.TaskTypeCard {
			if conflict, occupant := cardChildSlotConflict(parent, req.Ref, req.ProjectID, req.Behavior); conflict {
				return nil, &StatusError{
					Code: http.StatusConflict,
					Message: fmt.Sprintf(
						"create task: card %q's single work slot is already occupied by %s",
						req.ParentID, occupant),
				}
			}
		}
		// A parent lookup failure here is deliberately non-fatal: this is a
		// defense-in-depth check, not the parent existence check itself (a
		// genuinely missing/unreadable parent surfaces its own error further
		// down the ordinary create path, same posture as the remote_id
		// inheritance lookups above).
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
	if err := s.Tasks.CreateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	// Guard: only fire auto_start for a freshly pending task. When get-or-create
	// at the store level returns an existing task (e.g. concurrent create race,
	// or an IdempotencyKey hit landing on a still-pending task from a resumed
	// caller), the task may already be executing or terminal — this check
	// covers both Ref's and IdempotencyKey's get-or-create paths, since an
	// existing task that is executing/awaiting/done/aborted never re-fires
	// start either way.

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

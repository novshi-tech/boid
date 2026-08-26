package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// validateParentlessExecutorBase surfaces the "parent-less executor pointed
// at a non-existent base" error at task creation time. The supervisor-side
// case-1/2/3 → worktree routing that used to live here was removed in
// branch-policy-simplification Phase 2: every project-visible job now runs in
// a fresh sandbox clone, so no per-task worktree decision is needed. The
// executor guard is preserved because it catches a user-visible config bug
// (a top-level executor whose base_branch does not exist on origin) at
// creation time rather than deep inside the sandbox clone.
//
// The function is conservative: when classification itself fails (e.g.
// detached HEAD, project lookup unwired) the error is surfaced so callers
// cannot silently fall through to a broken sandbox run.
//
// Rationale for living on the service (rather than orchestrator pkg): the
// decision combines task-row metadata (behaviorName, parent), project meta
// (workdir lookup), and orchestrator primitives. Pushing it into orchestrator
// would require importing the ProjectWorkDirLookup interface back, which is
// the wrong direction for the layer boundary (orchestrator → api is forbidden
// per feedback_layer_boundary_enforcement). Service is the right join point.
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
// start from are allowed.
//
// docs/plans/suggestion-as-state-transition-impl.md §3.5: card machine v2 has
// no "captured"/"triaged" statuses at all (see machine_card.go's doc
// comment) — a card is born directly into "parked" now (§3.2's capture rule).
// captured/triaged are dropped from this allowlist and replaced by "parked".
// This is a deliberate breaking change to the initial_status vocabulary
// (khi is cut over to the same vocabulary in the same release, per the
// impl plan's PR sequencing — a captured/triaged request from an
// un-upgraded caller now 400s instead of silently creating a legacy-shaped
// card no rule in v2 can ever move again).
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

	// docs/plans/ingestion-identity.md PR-2 (B-2), J-10/A-5: description size
	// cap. One of the 4 mandatory entry points (task_create / task_update /
	// action_send / BoidOpTaskResolveOrCapture) — see
	// orchestrator.ValidateContentSize's own doc comment for the limit's
	// value and the real-world measurement it's based on.
	if err := orchestrator.ValidateContentSize("description", []byte(req.Description)); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// card-model-cleanup PR-2 (docs/plans/card-model-cleanup.md §3.7): a card
	// (initial_status=parked) never calls ResolveBehavior at all — Behavior/
	// Traits/Readonly/BranchPrefix/BaseBranch/Payload/Instructions/AutoStart
	// are execution-only fields a card structurally cannot carry (ExecAttrs
	// is nil), so there is nothing for behavior resolution to feed into. This
	// is what retires the base_branch-template landmine
	// task_resolve_or_capture.go's ResolveOrCapture used to carry (a
	// captured/parked task's BaseBranch held a literal, unexpanded template
	// string, dead data because card machine v2 has no rule reaching
	// "executing" directly) — the ExecAttrs split makes that state
	// unrepresentable instead of merely unreachable.
	if initialStatus == orchestrator.TaskStatusParked {
		return s.createCardTask(req, initialStatus)
	}
	return s.createExecutionTask(req, initialStatus)
}

// createCardTask builds and inserts a fresh Card (design doc §3.7's purified
// capture path, applied to the generic CreateTask entry point too — not just
// ResolveOrCapture). A card created here starts with empty CardAttrs (kind/
// urgency/wake_at/wake_task_id/suggestion_verb/detail all zero-valued) —
// exactly what SeedTaskTriage used to insert after the fact. Since the row
// is type='card' from the INSERT itself, there is no long a separate
// "seeding" step at all (design doc §3.6): a card cannot be born rowless.
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

func (s *TaskAppService) createExecutionTask(req CreateTaskRequest, initialStatus orchestrator.TaskStatus) (*orchestrator.Task, error) {
	var meta *orchestrator.ProjectMeta
	if s.Meta != nil {
		// Hydrate with workspace.yaml so a workspace-level default project
		// definition's task_behaviors are visible to ResolveBehavior, not
		// just project.yaml's own (docs/plans/workspace-default-project.md
		// §PR分割案 PR2, §現状の実測4's "task 作成" row — this was the one
		// call site among the 5 mandatory switches that still read bare
		// Meta.Get). Falls back to bare Get on any hydration failure — same
		// idiom as ProjectAppService.hydrateProjectWithWorkspace
		// (project_service.go) and TaskWorkflowService.ApplyAction
		// (workflow_action.go) already use. This preserves CreateTask's
		// pre-existing "meta not loaded → nil meta, continue" tolerance
		// (ResolveBehavior handles a nil meta unconditionally) for the
		// "meta not loaded" case, and additionally degrades gracefully
		// — rather than failing task creation outright — for the two NEW
		// failure modes GetWithWorkspace can produce that bare Get never
		// could (a corrupt workspace.yaml, a host_commands conflict): the
		// project's own project.yaml behaviors still work, just without
		// workspace-level enrichment.
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
	// Phase 2-3: task-row level overrides for worktree / base_branch /
	// branch_prefix have been removed. Values come from the resolved behavior
	// (and project-level defaults for worktree / base_branch).
	// readonly is now a first-class override: when supplied, it wins over the
	// behavior default set by applyCanonicalBehaviorOverrides.

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
		// P1 priority 2: a task with no base_branch → expand ${current_branch}.
		// Detached HEAD is surfaced as a 400.
		//
		// This used to be restricted to the canonical "supervisor"/"executor"
		// behaviors, on the theory that non-canonical (custom) behaviors were
		// allowed an empty baseBranch outright — see
		// classifyAndApplyBaseBranchCase's early return, which still applies
		// only to those two names (deciding worktree=true/false via
		// ClassifyBaseBranch is a canonical-behavior-only concern). That was
		// fine pre-cutover: worktree=false + empty BaseBranch just meant "run
		// in the project dir as-is". Post-cutover
		// (docs/plans/git-gateway-cutover.md PR6), every project-visible
		// dispatch needs a resolvable base_branch to build its sandbox-internal
		// CloneDeclaration (dispatcher.BuildCloneDeclaration reads
		// task.BaseBranch directly, with no non-canonical fallback), so an
		// empty BaseBranch on a non-canonical task now hard-fails the clone
		// deep inside the sandbox ("spec.Clone is enabled but
		// URL/TargetDir/Branch/BaseBranch must all be set") instead of
		// degrading gracefully. Expanding ${current_branch} regardless of
		// behavior name closes that gap; it only ever fires when baseBranch
		// was empty to begin with, so canonical-behavior tasks (which already
		// took this path) are unaffected.
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
		// P1 priority 3: explicit base → expand ${TASK_REMOTE_ID} first so a
		// missing remote_id errors out before we touch the project working
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
	// does not exist on origin. Post-Phase-2 there is no supervisor 3-case
	// worktree routing left to do here.
	if err := s.validateParentlessExecutorBase(req, res.BehaviorName, baseBranch); err != nil {
		return nil, err
	}

	// Get-or-create: when ref is present, check for an existing task (scoped
	// by parent_id, which is "" for a root task) before building and
	// inserting a new one. This is the service-level dedup guard; the store
	// has an identical check for the concurrent-create race. Returning early
	// here avoids a redundant INSERT round-trip.
	//
	// Phase 1 PR-4 (docs/plans/cross-project-issue-triage.md 論点7): this used
	// to require ParentID != "" (child-only dedup, PR-2's per-child Ref idiom
	// — see Dispatch's Ref: children[i].ID call). Opening it to root tasks
	// too is what makes ingestion push idempotent: khi creates a card with
	// Ref=<source_ref> (jira issue_key / slack thread_ts / mail message-id),
	// and a resend after a crash between the create response and khi
	// recording the returned task_id returns the SAME existing task instead
	// of a duplicate card. idx_tasks_ref_parent (migration 0010) already
	// covers parent_id="" rows uniquely: parent_id is `NOT NULL DEFAULT ''`
	// (never SQL NULL), so the unique index on (ref, parent_id) treats every
	// root task's ref as unique among root tasks the same way it already did
	// for a given parent's children — no migration was needed to open this.
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
	// SeedTaskTriage (the task_triage sidecar seed for a task created
	// directly into a pre-execution status) is GONE as of card-model-cleanup
	// PR-2 (design doc §3.6): a card is type='card' from the INSERT itself
	// now (createCardTask above), so there is no "seed it after the fact"
	// step left for this execution-task path to perform — an execution task
	// is never a card no matter what status it starts in.
	// Guard: only fire auto_start for a freshly pending task. When get-or-create
	// at the store level returns an existing task (e.g. concurrent create race),
	// the task may already be executing or terminal.
	//
	// Actor caveat (論点11): CreateTask(req CreateTaskRequest) has no ctx
	// parameter, so this always stamps ActorHuman even though this call also
	// backs `boid task create` from inside a sandbox (internal/server/
	// boid_executor.go's BoidOpTaskCreate) and Dispatch's child-task
	// creation (workflow_card.go), both of which should really carry the
	// creating task's own actor. Threading ctx through CreateTask/UpdateTask/
	// RerunTask (this whole family predates ctx) is a follow-up, not done
	// here to keep this PR's blast radius to the ctx seams that already
	// exist.
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

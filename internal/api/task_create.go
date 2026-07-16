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

func (s *TaskAppService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	var meta *orchestrator.ProjectMeta
	if s.Meta != nil {
		if m, ok := s.Meta.Get(req.ProjectID); ok {
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

	// Get-or-create: when ref and parent are both present, check for an existing
	// child before building and inserting a new task. This is the service-level
	// dedup guard; the store has an identical check for the concurrent-create
	// race. Returning early here avoids a redundant INSERT round-trip.
	if req.Ref != "" && req.ParentID != "" {
		existing, err := s.Tasks.FindTaskByRef(req.Ref, req.ParentID)
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
		ID:           req.ID,
		ProjectID:    req.ProjectID,
		Title:        req.Title,
		Description:  req.Description,
		Behavior:     res.BehaviorName,
		Traits:       traits,
		Readonly:     readonly,
		BranchPrefix: branchPrefix,
		BaseBranch:   baseBranch,
		RemoteID:     req.RemoteID,
		Payload:      payload,
		Instructions: res.Instructions,
		AutoStart:    req.AutoStart,
		Ref:          req.Ref,
		ParentID:     req.ParentID,
	}
	if err := s.Tasks.CreateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	// Guard: only fire auto_start for a freshly pending task. When get-or-create
	// at the store level returns an existing task (e.g. concurrent create race),
	// the task may already be executing or terminal.
	if req.AutoStart && s.Workflow != nil && task.Status == orchestrator.TaskStatusPending {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
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

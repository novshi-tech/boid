package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// classifyAndApplyBaseBranchCase performs the Phase 2-2 supervisor 3-case
// classification and adjusts task.Worktree based on the result. It is also
// where the "parent-less executor pointed at a non-existent base" error is
// surfaced — a child executor with a parent inherits the parent's already-
// resolved base (which by construction exists or will be created when the
// parent runs) and thus skips classification entirely.
//
// Returns the updated worktree flag (the task field) and a *StatusError on
// validation failure. The function is conservative: when classification
// itself fails (e.g. detached HEAD, project lookup unwired) it surfaces the
// error so callers cannot silently fall through to a broken supervisor run.
//
// Rationale for living on the service (rather than orchestrator pkg): the
// decision combines task-row metadata (behaviorName, parent), project meta
// (workdir lookup), and orchestrator primitives. Pushing it into orchestrator
// would require importing the ProjectWorkDirLookup interface back, which is
// the wrong direction for the layer boundary (orchestrator → api is forbidden
// per feedback_layer_boundary_enforcement). Service is the right join point.
func (s *TaskAppService) classifyAndApplyBaseBranchCase(req CreateTaskRequest, behaviorName, baseBranch string, worktree, inheritedFromParent bool) (bool, error) {
	// Inherited base branches were already validated when the parent was
	// scheduled; re-checking here would either double-trip (parent already
	// exists → case 2) or fight the parent's case-3 promise. Same reasoning
	// as the inheritance branch in CreateTask.
	if inheritedFromParent {
		return worktree, nil
	}
	if behaviorName != "supervisor" && behaviorName != "executor" {
		// Non-canonical behaviors keep the existing semantics (P3-1 will
		// remove the divergence entirely).
		return worktree, nil
	}
	if s.Projects == nil {
		// No project workdir lookup available (e.g. test wiring without a
		// Projects stub). Without it we cannot classify; leave the worktree
		// decision untouched and skip the check. CreateTask paths that need
		// the classification wire the Projects field — silent skipping here
		// matches the legacy behavior of the base_branch expander.
		return worktree, nil
	}
	proj, projErr := s.Projects.GetProject(req.ProjectID)
	if projErr != nil {
		return worktree, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project lookup failed: %v", projErr)}
	}
	if proj == nil || proj.WorkDir == "" {
		return worktree, nil
	}

	state, err := orchestrator.ClassifyBaseBranch(proj.WorkDir, baseBranch)
	if err != nil {
		return worktree, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("classify base_branch %q: %v", baseBranch, err)}
	}

	switch behaviorName {
	case "supervisor":
		// Supervisor case routing:
		//   case 1 → worktree=false (run in project dir)
		//   case 2 → worktree=true  (check out the existing base in a worktree)
		//   case 3 → worktree=true  (worktree manager will create the base)
		switch state {
		case orchestrator.Case1HeadMatches:
			worktree = false
		case orchestrator.Case2ExistsButNotCheckedOut, orchestrator.Case3NotFound:
			worktree = true
		}
	case "executor":
		// Executor never runs in the project dir, so case 1 / case 2 are both
		// fine. Case 3 with no parent is an error: a child executor inherits
		// its parent's base_branch (so its presence is the parent's
		// responsibility), but a parent-less executor has nobody to create
		// the missing base.
		if state == orchestrator.Case3NotFound && req.ParentID == "" {
			return worktree, &StatusError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf("executor base_branch %q does not exist locally or on origin, and the task has no parent supervisor to create it", baseBranch),
			}
		}
	}
	return worktree, nil
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
	worktree := res.Worktree
	branchPrefix := res.BranchPrefix
	baseBranch := res.BaseBranch
	payload := res.Payload

	if req.Traits != nil {
		traits = req.Traits
	}
	// Phase 2-3: task-row level overrides for readonly / worktree / base_branch
	// / branch_prefix have been removed. Values come from the resolved behavior
	// (and project-level defaults for worktree / base_branch).

	// Phase 1-3: parent-child base_branch inheritance.
	//
	// If the new task has a parent, inherit the parent's already-resolved
	// BaseBranch verbatim and skip both expanders. The parent's BaseBranch
	// was expanded when the parent was created, so re-expansion (especially
	// of ${TASK_REMOTE_ID}) on the child would diverge from the parent's
	// branch and break the worktree assumption that all children of a
	// task share its base.
	//
	// Static base_branch values (e.g. "main") on the child are also discarded
	// in favor of the parent's value: "parent's resolved base wins" is the
	// invariant. See README of Phase 1-3 for the rationale.
	//
	// Parent-not-found is logged and we fall through to behavior-level
	// expansion: legacy callers that pre-date strict parent validation
	// (and many existing tests) wire parent_ids that aren't real rows.
	// Phase 2 will tighten this once those callers are migrated.
	inheritedFromParent := false
	if req.ParentID != "" {
		parent, parentErr := s.Tasks.GetTask(req.ParentID)
		if parentErr != nil || parent == nil {
			slog.Warn("parent task not found for base_branch inheritance; falling back to behavior-level resolution",
				"scope", "CreateTask",
				"task_id", req.ID,
				"parent_id", req.ParentID,
				"error", parentErr,
			)
		} else {
			baseBranch = parent.BaseBranch
			inheritedFromParent = true
		}
	}

	if !inheritedFromParent {
		if baseBranch == "" && (res.BehaviorName == "supervisor" || res.BehaviorName == "executor") {
			// P1 priority 2: root canonical task with no base_branch → expand
			// ${current_branch}. Detached HEAD is surfaced as a 400. Non-canonical
			// behaviors are allowed an empty baseBranch (they bypass ClassifyBaseBranch).
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
	}

	// Phase 2-2: supervisor 3-case execution location decision + executor base
	// existence check. classifyAndApplyBaseBranchCase mutates worktree (and
	// short-circuits with an error in the "executor + case 3 + no parent" case);
	// inheritedFromParent skips the classify entirely because the parent's
	// base must already exist (case 1/2) or have been created (case 3) when
	// the parent itself was scheduled.
	worktree, err = s.classifyAndApplyBaseBranchCase(req, res.BehaviorName, baseBranch, worktree, inheritedFromParent)
	if err != nil {
		return nil, err
	}

	var resolvedDeps []string
	for _, dep := range req.DependsOn {
		t, err := s.Tasks.FindTaskByRef(dep, req.ParentID)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("depends_on: ref %q lookup failed: %v", dep, err)}
		}
		if t == nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("depends_on: ref %q not found (parent_id: %s)", dep, req.ParentID)}
		}
		resolvedDeps = append(resolvedDeps, t.ID)
	}

	task := &orchestrator.Task{
		ID:               req.ID,
		ProjectID:        req.ProjectID,
		Title:            req.Title,
		Description:      req.Description,
		Behavior:         res.BehaviorName,
		Traits:           traits,
		Readonly:         readonly,
		Worktree:         worktree,
		BranchPrefix:     branchPrefix,
		BaseBranch:       baseBranch,
		RemoteID:         req.RemoteID,
		DataSourceID:     req.DataSourceID,
		Payload:          payload,
		Instructions:     res.Instructions,
		AutoStart:        req.AutoStart,
		DependsOn:        resolvedDeps,
		DependsOnPayload: req.DependsOnPayload,
		Ref:              req.Ref,
		ParentID:         req.ParentID,
	}
	if err := s.Tasks.CreateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if req.AutoStart && s.Workflow != nil {
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
		if req.RemoteID == "" && req.DataSourceID == "" {
			result.Errors = append(result.Errors, ImportError{
				Line:     i + 1,
				RemoteID: req.RemoteID,
				Error:    "remote_id and datasource_id are required",
			})
			continue
		}

		existing, err := s.Tasks.FindTaskByRemote(req.RemoteID, req.DataSourceID)
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

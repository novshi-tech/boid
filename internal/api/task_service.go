package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type TaskAppService struct {
	Tasks       TaskStore
	Actions     ActionStore
	Jobs        JobStore
	Meta        MetaStore
	Workflow    WorkflowService
	Projects    ProjectWorkDirLookup
	RuntimesDir string
	Notify      Notifier
	// BlockingAsk coordinates harness-independent blocking Q&A (boid task ask).
	// Shared with the sandbox boid builtin executor (which calls AskTaskBlocking)
	// and the answer path (AnswerTask), so both halves of a blocking ask use the
	// same in-memory registry. Nil disables blocking ask (notify --ask still works).
	BlockingAsk *BlockingAskRegistry
	// AskDisconnectGrace is how long an awaiting task may sit with no live agent
	// parked before the daemon reclaims it (a blocking ask whose foreground
	// command was killed by a harness command-timeout). Zero falls back to
	// defaultAskDisconnectGrace.
	AskDisconnectGrace time.Duration
}

// Notifier sends an agent-driven notification for a task. Implementations
// typically exec a user-configured command. nil-safe at the call site:
// TaskAppService.NotifyTask returns an error when Notify is unset.
type Notifier interface {
	Notify(ctx context.Context, ev notify.Event) error
}

func (s *TaskAppService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	tasks, err := s.Tasks.ListTasks(filter)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if tasks == nil {
		tasks = []*orchestrator.Task{}
	}
	return tasks, nil
}

func (s *TaskAppService) GetTask(id string) (*orchestrator.Task, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return task, nil
}

// GetTaskField resolves a dotted field path against the task. See
// ResolveTaskField for the path syntax (top-level fields, payload traits,
// computed lifecycle).
func (s *TaskAppService) GetTaskField(id, path string) (string, error) {
	if path == "" {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "field path is required"}
	}
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return "", &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	value, err := ResolveTaskField(task, s.Actions, path)
	if err != nil {
		return "", &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return value, nil
}

func (s *TaskAppService) UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if req.Title != "" {
		if task.Status != orchestrator.TaskStatusPending {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit title while task is not pending (status: %s)", task.Status),
			}
		}
		task.Title = req.Title
	}
	if req.ProjectID != "" {
		if task.Status != orchestrator.TaskStatusPending {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit project while task is not pending (status: %s)", task.Status),
			}
		}
		if s.Projects != nil {
			if _, err := s.Projects.GetProject(req.ProjectID); err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project %q not found", req.ProjectID)}
			}
		}
		task.ProjectID = req.ProjectID
	}
	if req.Description != "" {
		task.Description = req.Description
	}
	if req.RemoteID != nil {
		task.RemoteID = *req.RemoteID
	}
	if len(req.Payload) > 0 {
		if err := orchestrator.RejectPayloadInstructions(req.Payload); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		if err := orchestrator.RejectReservedPayloadKeys(req.Payload); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		// 案 B: artifact.<handler-role> が別 top-level キーになるため、
		// top-level shallow merge で handler 間の書き込みが衝突しない。
		// null は削除。instructions の特別扱いは不要。
		var base map[string]json.RawMessage
		if len(task.Payload) > 0 && string(task.Payload) != "null" {
			if err := json.Unmarshal(task.Payload, &base); err != nil {
				return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload parse: " + err.Error()}
			}
		}
		if base == nil {
			base = make(map[string]json.RawMessage)
		}
		var override map[string]json.RawMessage
		if err := json.Unmarshal(req.Payload, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
		}
		for k, v := range override {
			if string(v) == "null" {
				delete(base, k)
			} else {
				base[k] = v
			}
		}
		merged, err := json.Marshal(base)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "payload merge: " + err.Error()}
		}
		task.Payload = merged
	}
	if req.ParentID != nil {
		task.ParentID = *req.ParentID
	}
	// Phase 2-3: task-row level base_branch / branch_prefix / worktree updates
	// have been removed. These values are determined at create time from the
	// behavior type and project-level defaults, and are no longer mutable.
	var instructionsBefore orchestrator.Instructions
	if len(req.Instructions) > 0 {
		if !orchestrator.IsInstructionsEditable(task.Status) {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit instructions while task is running (status: %s)", task.Status),
			}
		}
		instructionsBefore = cloneInstructions(task.Instructions)
		var override orchestrator.Instructions
		if err := json.Unmarshal(req.Instructions, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions parse: " + err.Error()}
		}
		task.Instructions = override
	}
	if req.AutoStart != nil {
		task.AutoStart = *req.AutoStart
	}
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if instructionsBefore != nil {
		s.auditInstructionsChange(task.ID, instructionsBefore, task.Instructions)
	}
	if req.AutoStart != nil && *req.AutoStart && task.Status == orchestrator.TaskStatusPending && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("auto_start: update: failed to apply start action", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}
	return task, nil
}

// UpdateTaskPayloadPatch applies patch to the task owning jobID using the
// SAME merge semantics the file-based payload_patch.json → job_done →
// Coordinator pipeline has always applied (orchestrator.MergePayloadPatch,
// gated by the trait allowlist the firing hook itself declares via
// Traits.Produces) — see orchestrator/coordinator.go's
// HandlerResult.allowedTraits and wiring-seams.md #13/#17. This is
// deliberately NOT UpdateTask's simpler top-level shallow merge (used by
// --payload-file): UpdateTask has no notion of a "firing hook", so it can't
// reproduce this gate.
//
// jobID (not taskID) is the identity this method resolves from, because the
// allowedTraits gate is keyed off the CALLING job's own HandlerID — the
// specific hook that was dispatched to produce this job, which may differ
// from other jobs the same task has had or will have (mirrors why
// BoidOpTaskInstructions/Env/Payload are JobID-scoped, not TaskID-scoped).
//
// allowedTraits is supplied by the CALLER — it must be the value captured
// AT DISPATCH TIME (dispatcher.JobContextSnapshot.PayloadPatchAllowedTraits,
// itself sourced from orchestrator.JobSpec.HookTraitsProduces), never
// re-derived here from a live project-meta lookup. An earlier version of
// this method did its own live lookup (GetTask's ProjectID -> current meta
// -> current behavior -> hook by HandlerID) and codex review caught the
// TOCTOU staleness bug that creates: if project.yaml is edited/reloaded
// between dispatch and this call, a live lookup can either apply the WRONG
// (post-edit) trait list or silently fall back to unrestricted when the
// hook can no longer be found by name — neither matches what was actually
// authorized when the job was dispatched. Accepting the dispatch-time value
// as a parameter makes that class of staleness structurally impossible
// instead of requiring a "fail closed on lookup failure" special case (see
// wiring-seams.md #17's Major 1 finding). nil means unrestricted — see
// JobSpec.HookTraitsProduces's own doc comment for exactly when that's the
// correct (not just the fallback) value.
//
// The GetTask -> MergePayloadPatch -> UpdateTask sequence below is
// serialized per task id (payloadPatchLockFor) so two concurrent calls for
// the same task — e.g. two hooks in the same readonly task's parallel
// dispatch round, each patching a different trait — cannot race a
// read-modify-write and silently lose one of their writes (Phase 5b PR7
// codex review Blocker 2, wiring-seams.md #17).
func (s *TaskAppService) UpdateTaskPayloadPatch(jobID string, patch json.RawMessage, allowedTraits []orchestrator.TraitType) (*orchestrator.Task, error) {
	if s.Jobs == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "job store unavailable"}
	}
	job, err := s.Jobs.GetJob(jobID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	lock := payloadPatchLockFor(job.TaskID)
	lock.Lock()
	defer lock.Unlock()

	task, err := s.Tasks.GetTask(job.TaskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	merged, err := orchestrator.MergePayloadPatch(task.Payload, patch, job.HandlerID, allowedTraits)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	task.Payload = merged
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return task, nil
}

func (s *TaskAppService) DeleteTask(id string, force bool) error {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if !force {
		if task.Status == orchestrator.TaskStatusExecuting {
			return &StatusError{
				Code:    http.StatusConflict,
				Message: "task is active (status: " + string(task.Status) + "); use --force to delete",
			}
		}
	}
	if err := s.Tasks.DeleteTask(id); err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return nil
}

func (s *TaskAppService) DuplicateTask(sourceID string, autoStart bool) (*orchestrator.Task, error) {
	source, err := s.GetTask(sourceID)
	if err != nil {
		return nil, err
	}
	req := CreateTaskRequest{
		ProjectID:   source.ProjectID,
		Title:       source.Title,
		Description: source.Description,
		Behavior:    source.Behavior,
		RemoteID:    source.RemoteID,
		Traits:      source.Traits,
		AutoStart:   autoStart,
	}
	// Carry the source's instructions (e.g. a per-project release-policy override)
	// so the duplicate behaves identically. RemoteID in particular must be copied:
	// a base_branch template such as "feature/${TASK_REMOTE_ID}" cannot resolve
	// without it, so a duplicate that dropped remote_id failed outright. Leave
	// Instructions unset when the source has none, so CreateTask falls back to the
	// behavior's default_instruction.
	//
	// Ref is deliberately NOT copied. It is a within-parent identity key guarded by
	// the partial unique index idx_tasks_ref_parent (ref, parent_id) WHERE ref != ''.
	// Copying a non-empty source ref into a sibling with the same parent_id collides
	// on that index, so duplicating any task that carries a ref (e.g. a re-duplicated
	// supervisor) failed outright. A duplicate is a brand-new task and must get its
	// own ref scope: CreateTask leaves it empty for a root task or auto-generates a
	// fresh unique ref for a child. Multiple tasks per remote_id are expected
	// (one issue can spawn several tasks), so nothing here should be unique-keyed.
	if len(source.Instructions) > 0 {
		raw, err := json.Marshal(source.Instructions)
		if err != nil {
			return nil, fmt.Errorf("marshal source instructions: %w", err)
		}
		req.Instructions = raw
	}
	return s.CreateTask(req)
}

func (s *TaskAppService) RerunTask(id string, req RerunTaskRequest) (*orchestrator.Task, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not in a rerun-able state (status: %s)", task.Status),
		}
	}

	var instructionsBefore orchestrator.Instructions
	if len(req.InstructionsOverride) > 0 && string(req.InstructionsOverride) != "null" {
		instructionsBefore = cloneInstructions(task.Instructions)
		var override orchestrator.Instructions
		if err := json.Unmarshal(req.InstructionsOverride, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions parse: " + err.Error()}
		}
		task.Instructions = override
	}

	task.Status = orchestrator.TaskStatusPending
	task.Payload = json.RawMessage("{}")
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	if instructionsBefore != nil {
		s.auditInstructionsChange(task.ID, instructionsBefore, task.Instructions)
	}

	if req.AutoStart && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("rerun auto_start: failed to apply start action", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}

	return task, nil
}

func cloneInstructions(src orchestrator.Instructions) orchestrator.Instructions {
	if src == nil {
		return nil
	}
	out := make(orchestrator.Instructions, len(src))
	copy(out, src)
	return out
}

// auditInstructionsChange records an instructions change as an Action so that
// the reason behind rerun-over-rerun outcome differences can be traced.
func (s *TaskAppService) auditInstructionsChange(taskID string, before, after orchestrator.Instructions) {
	if s.Actions == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"before": before,
		"after":  after,
	})
	if err != nil {
		slog.Error("audit instructions change: marshal", "task_id", taskID, "error", err)
		return
	}
	action := &orchestrator.Action{
		TaskID:  taskID,
		Type:    "update_instructions",
		Payload: payload,
	}
	if err := s.Actions.CreateAction(action); err != nil {
		slog.Error("audit instructions change: create action", "task_id", taskID, "error", err)
	}
}

func (s *TaskAppService) GetTaskDetail(id string) (*TaskDetailView, error) {
	task, err := s.GetTask(id)
	if err != nil {
		return nil, err
	}

	actions, err := s.Actions.ListActionsByTask(task.ID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	jobs, err := s.Jobs.ListJobsByTask(task.ID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, j := range jobs {
		enrichJob(s.RuntimesDir, j)
		enrichJobDisplayName(j, task.Behavior, s.Meta)
	}

	return &TaskDetailView{
		Task:             task,
		Actions:          actions,
		Jobs:             jobs,
		AvailableActions: orchestrator.DefaultMachine().AvailableActions(task.Status),
	}, nil
}

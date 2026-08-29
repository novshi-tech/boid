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
	// TaskWaits is the registry the brokered task_wait op records itself in
	// while parked, so the trigger sweep can tell which task a job is waiting
	// on. Shared with TaskWorkflowService (wire.go builds one). Nil disables the
	// attribution; the wait itself still works.
	TaskWaits *TaskWaitRegistry
	// WaitPollInterval is how often WaitTaskTerminal re-reads a task row while
	// blocked on it. Zero falls back to defaultWaitPollInterval — same
	// convention as AskDisconnectGrace above.
	WaitPollInterval time.Duration
	// Identities backs docs/plans/ingestion-identity.md PR-1 (B-1)'s identity
	// index — the non-transactional path the brokered task_identity_link /
	// _unlink / _resolve ops (boid_executor.go) call through. The drop side
	// effect (I-6) instead goes through TxStore.UnlinkAllForTask inside
	// TaskWorkflowService.ApplyAction's own transaction; this field is only
	// for the standalone ops, which have no transaction of their own to join.
	// Nil disables the three ops with an "unavailable" error, same convention
	// as every other optional dependency here.
	Identities TaskIdentityStore
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
		if !orchestrator.IsPreDispatchEditableStatus(task.Type, task.Status) {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit title while task is not pending/pre-dispatch (status: %s)", task.Status),
			}
		}
		task.Title = req.Title
	}
	if req.ProjectID != "" {
		if !orchestrator.IsPreDispatchEditableStatus(task.Type, task.Status) {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit project while task is not pending/pre-dispatch (status: %s)", task.Status),
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
		// docs/plans/ingestion-identity.md PR-2 (B-2), J-10/A-5: same
		// description size cap as CreateTask — see
		// orchestrator.ValidateContentSize's own doc comment.
		if err := orchestrator.ValidateContentSize("description", []byte(req.Description)); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		task.Description = req.Description
	}
	if req.RemoteID != nil {
		task.RemoteID = *req.RemoteID
	}
	if len(req.Payload) > 0 {
		// Payload is execution-only (design doc §3.2) — a card has no field
		// to merge this into at all (ExecAttrs doesn't exist on it).
		if task.Exec == nil {
			return nil, &StatusError{Code: http.StatusConflict, Message: "cannot edit payload: task is a card (payload is execution-only)"}
		}
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
		if len(task.Exec.Payload) > 0 && string(task.Exec.Payload) != "null" {
			if err := json.Unmarshal(task.Exec.Payload, &base); err != nil {
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
		task.Exec.Payload = merged
	}
	if req.ParentID != nil {
		task.ParentID = *req.ParentID
	}
	// Phase 2-3: task-row level base_branch / branch_prefix / worktree updates
	// have been removed. These values are determined at create time from the
	// behavior type and project-level defaults, and are no longer mutable.
	var instructionsBefore orchestrator.Instructions
	if len(req.Instructions) > 0 {
		// IsInstructionsEditable(task.Type, task.Status) is false for ANY
		// card status (Instructions is execution-only, design doc §3.2) —
		// same 409 a card already gets from every other status this used to
		// reject, no separate task.Exec nil-check needed before the
		// task.Exec.Instructions write below.
		if !orchestrator.IsInstructionsEditable(task.Type, task.Status) {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot edit instructions while task is running (status: %s)", task.Status),
			}
		}
		instructionsBefore = cloneInstructions(task.Exec.Instructions)
		var override orchestrator.Instructions
		if err := json.Unmarshal(req.Instructions, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions parse: " + err.Error()}
		}
		task.Exec.Instructions = override
	}
	if req.AutoStart != nil {
		// AutoStart is execution-only too; unlike Instructions there is no
		// separate "editable status" gate to lean on (auto_start could
		// always be set regardless of status), so this needs its own guard.
		if task.Exec == nil {
			return nil, &StatusError{Code: http.StatusConflict, Message: "cannot edit auto_start: task is a card (auto_start is execution-only)"}
		}
		task.Exec.AutoStart = *req.AutoStart
	}
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if instructionsBefore != nil {
		s.auditInstructionsChange(task.ID, instructionsBefore, task.Exec.Instructions)
	}
	// Actor caveat (論点11): UpdateTask has no ctx, so this always stamps
	// ActorHuman even though it's also reachable from a sandbox
	// (OpBoidTaskUpdate). See CreateTask's matching comment (task_create.go)
	// — threading ctx through is a follow-up.
	if req.AutoStart != nil && *req.AutoStart && task.Status == orchestrator.TaskStatusPending && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), task.ID, ApplyActionRequest{Type: "start"})
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
	// A job's task is always an execution task (a card never dispatches a
	// job — design doc §3, cards run no agent sessions), so task.Exec is
	// expected non-nil here; this is defense in depth, not evidence a card
	// job is possible.
	if task.Exec == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "task is not an execution task (no Exec attrs); cannot apply a payload patch"}
	}

	merged, err := orchestrator.MergePayloadPatch(task.Exec.Payload, patch, job.HandlerID, allowedTraits)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	task.Exec.Payload = merged
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
		RemoteID:    source.RemoteID,
		AutoStart:   autoStart,
	}
	// Behavior/Traits/Instructions are execution-only (design doc §3.2) — a
	// card source has none to copy. CreateTask always builds the duplicate
	// as a fresh execution task (no InitialStatus is set below, so it
	// defaults to "pending"), so leaving these empty for a card source just
	// means the duplicate resolves the project's DEFAULT behavior instead of
	// "inheriting" one the source never had — the card-model-cleanup PR-2
	// design doc §7 calls this exact class of borrowed-but-meaningless
	// exec-field value ("嘘の値") out as something the ExecAttrs split is
	// SUPPOSED to make impossible, not a regression to work around.
	if source.Exec != nil {
		req.Behavior = source.Exec.Behavior
		req.Traits = source.Exec.Traits
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
	if source.Exec != nil && len(source.Exec.Instructions) > 0 {
		raw, err := json.Marshal(source.Exec.Instructions)
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
	// Rerun is an execution-task-only operation (it resets status to
	// "pending" and edits Instructions, both execution-only concepts —
	// design doc §3.2). A card can share the "done" status value with an
	// execution task (§3.3: the shared string is not ambiguous once type is
	// checked directly, unlike the old status-only machineFor guess this
	// refactor retires), so this type check is required, not merely
	// defensive: without it a done CARD's rerun would try to reset it to
	// "pending", a status migration 0045's CHECK constraint rejects for a
	// card row outright.
	if task.Type != orchestrator.TaskTypeExecution {
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not an execution task (type: %s); cannot rerun", task.Type),
		}
	}
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not in a rerun-able state (status: %s)", task.Status),
		}
	}

	var instructionsBefore orchestrator.Instructions
	if len(req.InstructionsOverride) > 0 && string(req.InstructionsOverride) != "null" {
		instructionsBefore = cloneInstructions(task.Exec.Instructions)
		var override orchestrator.Instructions
		if err := json.Unmarshal(req.InstructionsOverride, &override); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "instructions parse: " + err.Error()}
		}
		task.Exec.Instructions = override
	}

	task.Status = orchestrator.TaskStatusPending
	task.Exec.Payload = json.RawMessage("{}")
	if err := s.Tasks.UpdateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	if instructionsBefore != nil {
		s.auditInstructionsChange(task.ID, instructionsBefore, task.Exec.Instructions)
	}

	if req.AutoStart && s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), task.ID, ApplyActionRequest{Type: "start"})
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
		Actor:   orchestrator.ActorHuman,
	}
	// context.Background(): this call site has no ctx of its own (existing
	// pre-PR-1 gap — see UpdateTask's own "Actor caveat" comment above for
	// the same limitation on Actor) and Instructions is execution-only
	// (never a card), so the target axis excludes this write from ingest
	// regardless (docs/plans/boid-internal-signal-inbox.md §4.2).
	if err := s.Actions.CreateAction(context.Background(), action); err != nil {
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
		enrichJobDisplayName(j, taskBehaviorOrEmpty(task), s.Meta)
	}

	// PR-B (docs/plans/suggestion-as-state-transition-impl.md §2): task
	// detail is a generic per-task view (any task, card or ordinary), so the
	// governing machine is resolved dynamically. card-model-cleanup PR-2
	// retired machineForDisplay: machineFor is now a pure function of
	// task.Type (no DB lookup, so nothing left to fail transiently — see
	// machineFor's own doc comment).
	sm := machineFor(task)

	return &TaskDetailView{
		Task:             task,
		Actions:          actions,
		Jobs:             jobs,
		AvailableActions: sm.AvailableActions(task.Status),
	}, nil
}

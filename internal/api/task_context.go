package api

import (
	"encoding/json"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md「PR 分割案 > 5b」):
// task-context RPCs pulled live over the broker instead of materialized once
// at dispatch time as $HOME/.boid/context/*.{yaml,json} files. This file
// covers GetTaskCurrent (the `boid task current` RPC — safe to re-derive
// live from the task row, no job-scoped ambiguity) and GetInstructions (kept
// for potential task-row-level callers, e.g. a future Web UI "what will this
// task's active routing do next" view — see its own doc comment for why it
// is explicitly NOT the `boid task instructions` RPC's data source).
// `boid task instructions`, `boid task env`, and `boid task payload` are
// backed by dispatcher.Runner's per-job JobContextSnapshot instead — see
// internal/server/boid_executor.go's BoidOpTaskInstructions case and
// jobContextProvider's doc comment for why the split exists (this package
// has no way to tell apart two agent-kind hooks dispatched from the same
// task in one evaluation round; env has no DB representation at all; and
// payload needs the firing hook's trait Consumes list, which only exists at
// dispatch/plan time).

// GetTaskCurrent returns the task's business-metadata snapshot — the same
// subset historically materialized at $HOME/.boid/context/task.yaml
// (orchestrator.SnapshotTask), now also the payload of `boid task current`.
// Unlike the file (frozen at dispatch time), this re-derives from the task
// row on every call, so it reflects concurrent `task update` calls.
func (s *TaskAppService) GetTaskCurrent(id string) (*orchestrator.TaskSnapshot, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return orchestrator.SnapshotTask(task), nil
}

// GetTaskCurrentField resolves a dotted path against the task-current
// snapshot, mirroring GetTaskField's semantics for `--field` (missing path →
// "", scalar → unquoted/stringified, object/array → compact JSON).
func (s *TaskAppService) GetTaskCurrentField(id, path string) (string, error) {
	if path == "" {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "field path is required"}
	}
	snap, err := s.GetTaskCurrent(id)
	if err != nil {
		return "", err
	}
	return resolveMarshaledField(snap, path)
}

// GetInstructions returns the task's "active" routed instruction — the
// entry orchestrator.CurrentInstructions derives from the last entry in the
// task's instruction history.
//
// NOT the `boid task instructions` RPC's data source (fixed in codex review
// on PR #797 before merge — see wiring-seams.md #13 and
// internal/server/boid_executor.go's BoidOpTaskInstructions case): a task
// row has no notion of "which job is asking", but orchestrator.Evaluator can
// fire two agent-kind hooks for different agents from the same task in one
// round (any agent appearing anywhere in the instruction history matches,
// not just the active/last entry) — GetInstructions would hand a claude
// job's RPC call the codex hook's instruction whenever codex happens to be
// the most recent history entry, and vice versa. Kept as a task-row-level
// projection for callers that genuinely want "the task's current routing
// state" independent of any specific job (there are none yet in this
// codebase; a plausible future one is a Web UI task-detail view). Returns an
// empty (non-nil) slice rather than nil, even when the task carries no
// active instruction (not executing, or no instructions history yet).
func (s *TaskAppService) GetInstructions(id string) ([]orchestrator.RoutedInstruction, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	list := orchestrator.CurrentInstructions(task)
	if list == nil {
		list = []orchestrator.RoutedInstruction{}
	}
	return list, nil
}

// GetInstructionsField resolves a dotted path against the []RoutedInstruction
// list returned by GetInstructions, mirroring GetTaskField's --field contract.
func (s *TaskAppService) GetInstructionsField(id, path string) (string, error) {
	if path == "" {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "field path is required"}
	}
	list, err := s.GetInstructions(id)
	if err != nil {
		return "", err
	}
	return resolveMarshaledField(list, path)
}

// resolveMarshaledField JSON-marshals v and resolves path against it via
// ResolveJSONField, wrapping errors as the same *StatusError shapes
// GetTaskField uses (400 for a bad path, 500 for an internal marshal
// failure — the latter should be unreachable for the fixed-shape values this
// package passes in).
func resolveMarshaledField(v any, path string) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	value, err := ResolveJSONField(raw, path)
	if err != nil {
		return "", &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return value, nil
}

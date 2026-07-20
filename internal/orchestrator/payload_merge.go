package orchestrator

import (
	"encoding/json"
	"fmt"
)

// RejectPayloadInstructions returns an error if payload contains an "instructions" top-level key.
// instructions moved out of payload into Task.Instructions; accepting it here would silently drop it.
func RejectPayloadInstructions(payload json.RawMessage) error {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	if _, ok := m["instructions"]; ok {
		return fmt.Errorf(`"instructions" must be provided at top level, not inside payload`)
	}
	return nil
}

// MergeDefaultInstructions builds the initial instruction list for a new task.
//
// Merge semantics:
//   - defaultInstruction == nil: use requestInstructions as-is (no base to inherit from).
//   - defaultInstruction != nil:
//   - override empty/null/[]/{}  → return the default as a single-entry list.
//   - override has exactly 1 entry → per-field merge: non-empty fields from the
//     override win; empty fields inherit from defaultInstruction.
//   - override has 2+ entries → complete replacement (caller is building an
//     explicit history and partial merge would be ambiguous).
//
// requestInstructions accepts the array form `[{...}, ...]` and the legacy
// single-map form `{"main": {...}}` (handled by Instructions.UnmarshalJSON).
func MergeDefaultInstructions(defaultInstruction *Instruction, requestInstructions json.RawMessage) (Instructions, error) {
	var base Instructions
	if defaultInstruction != nil {
		base = Instructions{*defaultInstruction}
	}
	if len(requestInstructions) == 0 || string(requestInstructions) == "null" || string(requestInstructions) == "{}" || string(requestInstructions) == "[]" {
		return base, nil
	}
	var override Instructions
	if err := json.Unmarshal(requestInstructions, &override); err != nil {
		return nil, fmt.Errorf("unmarshal instructions: %w", err)
	}
	if len(override) == 0 {
		return base, nil
	}
	if defaultInstruction != nil && len(override) == 1 {
		merged := mergeInstruction(*defaultInstruction, override[0])
		return Instructions{merged}, nil
	}
	return override, nil
}

// mergeInstruction returns a new Instruction where non-empty fields from
// override replace the corresponding fields in base, and empty fields in
// override inherit from base.
func mergeInstruction(base, override Instruction) Instruction {
	out := base
	if override.Agent != "" {
		out.Agent = override.Agent
	}
	if override.Name != "" {
		out.Name = override.Name
	}
	if override.Message != "" {
		out.Message = override.Message
	}
	if override.Model != "" {
		out.Model = override.Model
	}
	return out
}

// AppendInstruction returns a new instruction list with `inst` appended. The
// caller is responsible for filling in fields not derived from the previous
// active entry. Used by `boid task reopen` to record a new context message
// while preserving history.
func AppendInstruction(existing Instructions, inst Instruction) Instructions {
	out := make(Instructions, len(existing)+1)
	copy(out, existing)
	out[len(existing)] = inst
	return out
}

// defaultInstructionMessage is the fallback dispatched to an agent when an
// instruction omits its message field.
const defaultInstructionMessage = "タスクを実行してください"

// resolveMessage returns the message for an instruction, falling back to the
// default when the message field is empty.
func resolveMessage(inst Instruction) string {
	if inst.Message != "" {
		return inst.Message
	}
	return defaultInstructionMessage
}

// FilterInstructions returns the active routed instruction for the given
// agent. Only the most recent entry in the history is considered (older
// entries are kept for audit but do not drive dispatch). Returns nil when
// agent is empty or the active entry does not address it. Callers gate on
// status==executing before routing (see selectInstruction).
func FilterInstructions(instructions Instructions, agent string) []RoutedInstruction {
	if agent == "" || len(instructions) == 0 {
		return nil
	}
	active := instructions[len(instructions)-1]
	if active.Agent != agent {
		return nil
	}
	return []RoutedInstruction{{
		Role:    active.Name,
		Agent:   active.Agent,
		Name:    active.Name,
		Message: resolveMessage(active),
		Model:   active.Model,
	}}
}

// CurrentInstructions returns the task's "active" routed instruction — the
// history's last entry, re-wrapped as a []RoutedInstruction via
// FilterInstructions(instructions, active.Agent).
//
// NOT a substitute for a specific job's routed instruction
// (selectInstruction in planner.go / JobSpec.Instruction), and NOT the data
// source for the Phase 5b `boid task instructions` broker RPC
// (docs/plans/phase5-shim-and-task-context.md) — see
// dispatcher.JobContextSnapshot's doc comment and wiring-seams.md #13 for
// the full story. The two agree only when exactly one agent-kind hook is
// live for the task's current phase. Evaluator.Evaluate can fire two
// agent-kind hooks for different agents from the same task in a single
// round — any agent appearing anywhere in the instruction history matches
// (extractInstructionAgents), not just the active/last entry — while
// selectInstruction/FilterInstructions only route the *last* entry, so the
// other hook's JobSpec.Instruction is nil. This function has no way to tell
// those two hooks' jobs apart: it always answers with whichever agent
// happens to be the history's last entry, regardless of which job asked.
// Safe as a task-row-level "what is this task's routing state right now"
// projection (no caller in this codebase uses it that way yet), unsafe as
// an answer to "what should THIS job be doing".
func CurrentInstructions(task *Task) []RoutedInstruction {
	if task == nil || task.Status != TaskStatusExecuting || len(task.Instructions) == 0 {
		return nil
	}
	active := task.Instructions[len(task.Instructions)-1]
	return FilterInstructions(task.Instructions, active.Agent)
}

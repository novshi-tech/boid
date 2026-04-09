package orchestrator

import "encoding/json"

type Evaluator struct{}

// InstructionTypeForStatus maps a task status to the corresponding InstructionType.
func InstructionTypeForStatus(status TaskStatus) InstructionType {
	switch status {
	case TaskStatusExecuting, TaskStatusCollectingFeedback:
		return InstructionTypeExecution
	case TaskStatusReworking:
		return InstructionTypeRework
	case TaskStatusVerifying, TaskStatusInReview:
		return InstructionTypeVerification
	default:
		return ""
	}
}

// extractInstructionConsumers returns the set of consumer names that have an
// instruction of the given type in the payload.
func extractInstructionConsumers(payload json.RawMessage, instType InstructionType) map[string]bool {
	if instType == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	raw, ok := m["instructions"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var instructions map[string]Instruction
	if err := json.Unmarshal(raw, &instructions); err != nil {
		return nil
	}
	consumers := make(map[string]bool)
	for _, inst := range instructions {
		if inst.Type == instType {
			consumers[inst.Consumer] = true
		}
	}
	if len(consumers) == 0 {
		return nil
	}
	return consumers
}

// consumesInstructions reports whether the handler traits include the instructions trait.
func consumesInstructions(traits HandlerTraits) bool {
	for _, t := range traits.Consumes {
		if t.Base() == TraitInstructions {
			return true
		}
	}
	return false
}

// Evaluate returns hooks that should fire for the given task.
func (e *Evaluator) Evaluate(task *Task, hooks []Hook) []Hook {
	activeTraits, _ := ActiveTraitTypes(task.Payload)
	traitSet := make(map[TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	instType := InstructionTypeForStatus(task.Status)
	consumers := extractInstructionConsumers(task.Payload, instType)

	var matched []Hook
	for _, h := range hooks {
		if !h.On.Contains(string(task.Status)) {
			continue
		}
		if h.Behavior != "" && h.Behavior != task.Behavior {
			continue
		}
		if !hasAllTraits(traitSet, h.Traits.Consumes) {
			continue
		}
		if consumesInstructions(h.Traits) {
			if instType == "" {
				continue
			}
			if h.Consumer == "" {
				continue // loader validation 後は到達しない想定
			}
			if !consumers[h.Consumer] {
				continue
			}
		}
		matched = append(matched, h)
	}
	return matched
}

// EvaluateGates returns gates that should fire for the given task.
// Unlike hooks, multiple gates may match the same state (kit composition).
func (e *Evaluator) EvaluateGates(task *Task, gates []Gate) []Gate {
	activeTraits, _ := ActiveTraitTypes(task.Payload)
	traitSet := make(map[TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []Gate
	for _, g := range gates {
		if !g.On.Contains(string(task.Status)) {
			continue
		}
		if g.Behavior != "" && g.Behavior != task.Behavior {
			continue
		}
		if !hasAllTraits(traitSet, g.Traits.Consumes) {
			continue
		}
		matched = append(matched, g)
	}
	return matched
}

func hasAllTraits(set map[TraitType]bool, required []TraitType) bool {
	for _, t := range required {
		if t.IsOptional() {
			continue
		}
		if !set[t] {
			return false
		}
	}
	return true
}

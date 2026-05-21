package orchestrator

type Evaluator struct{}

// InstructionTypeForStatus maps a task status to the corresponding InstructionType.
// Only executing tasks have an instruction type now that verifying/reworking are removed.
func InstructionTypeForStatus(status TaskStatus) InstructionType {
	if status == TaskStatusExecuting {
		return InstructionTypeExecution
	}
	return ""
}

// extractInstructionAgents returns the set of agent names that appear
// in the task's instruction history matching the given type. Empty type or
// empty list yields nil.
func extractInstructionAgents(instructions Instructions, instType InstructionType) map[string]bool {
	if instType == "" || len(instructions) == 0 {
		return nil
	}
	agents := make(map[string]bool)
	for _, inst := range instructions {
		if inst.Type == "" || inst.Type == instType {
			agents[inst.Agent] = true
		}
	}
	if len(agents) == 0 {
		return nil
	}
	return agents
}

// Evaluate returns hooks that should fire for the given task.
// Hooks fire only during executing state. Hooks with Kind == HandlerKindAgent
// additionally require an instruction in task.Instructions addressed to that
// hook's Agent.
func (e *Evaluator) Evaluate(task *Task, hooks []Hook) []Hook {
	if task.Status != TaskStatusExecuting {
		return nil
	}
	activeTraits, _ := ActiveTraitTypes(task.Payload)
	traitSet := make(map[TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	instType := InstructionTypeForStatus(task.Status)
	agents := extractInstructionAgents(task.Instructions, instType)

	var matched []Hook
	for _, h := range hooks {
		if !hasAllTraits(traitSet, h.Traits.Consumes) {
			continue
		}
		if h.Kind == HandlerKindAgent {
			if instType == "" {
				continue
			}
			if h.Agent == "" {
				continue // loader validation 後は到達しない想定
			}
			if !agents[h.Agent] {
				continue
			}
		}
		matched = append(matched, h)
	}
	return matched
}

// hasAllTraits checks whether all required traits are present in the set.
// Instructions routing is handled via HandlerKindAgent, not via traits.
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

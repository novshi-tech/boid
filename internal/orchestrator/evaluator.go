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

// extractInstructionConsumers returns the set of consumer names that appear
// in the task's instruction history matching the given type. Empty type or
// empty list yields nil.
func extractInstructionConsumers(instructions Instructions, instType InstructionType) map[string]bool {
	if instType == "" || len(instructions) == 0 {
		return nil
	}
	consumers := make(map[string]bool)
	for _, inst := range instructions {
		if inst.Type == "" || inst.Type == instType {
			consumers[inst.Consumer] = true
		}
	}
	if len(consumers) == 0 {
		return nil
	}
	return consumers
}

// Evaluate returns hooks that should fire for the given task.
// Hooks fire only during executing state. Hooks with Kind == HandlerKindAgent
// additionally require an instruction in task.Instructions addressed to that
// hook's Consumer.
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
	consumers := extractInstructionConsumers(task.Instructions, instType)

	var matched []Hook
	for _, h := range hooks {
		if !hasAllTraits(traitSet, h.Traits.Consumes) {
			continue
		}
		if h.Kind == HandlerKindAgent {
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

// EvaluateGates returns gates that should fire for the given task and phase.
// Entry gates fire when transitioning into executing (status=pending),
// exit gates fire when transitioning out of executing (status=executing).
// Multiple gates may match (kit composition).
func (e *Evaluator) EvaluateGates(task *Task, gates []Gate, phase GatePhase) []Gate {
	activeTraits, _ := ActiveTraitTypes(task.Payload)
	traitSet := make(map[TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []Gate
	for _, g := range gates {
		gPhase := g.Phase
		if gPhase == "" {
			gPhase = GatePhaseExit
		}
		if gPhase != phase {
			continue
		}
		if !hasAllTraits(traitSet, g.Traits.Consumes) {
			continue
		}
		matched = append(matched, g)
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

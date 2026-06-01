package orchestrator

type Evaluator struct{}

// extractInstructionAgents returns the set of agent names that appear in the
// task's instruction history. Empty list yields nil. Routing is gated on
// status==executing by the callers (Evaluate / selectInstruction), so no
// per-instruction phase filter is needed here.
func extractInstructionAgents(instructions Instructions) map[string]bool {
	if len(instructions) == 0 {
		return nil
	}
	agents := make(map[string]bool)
	for _, inst := range instructions {
		agents[inst.Agent] = true
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

	// status == executing is guaranteed above, so every instruction in the
	// history is live for routing; just collect the agents it addresses.
	agents := extractInstructionAgents(task.Instructions)

	var matched []Hook
	for _, h := range hooks {
		if !hasAllTraits(traitSet, h.Traits.Consumes) {
			continue
		}
		if h.Kind == HandlerKindAgent {
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

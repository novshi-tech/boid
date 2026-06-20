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
//
// Phase 3-e fallback: when the behavior declares no agent-kind hook at all
// (typical after the boid-kits claude-code/codex retirement landed in PR
// #604), the evaluator synthesizes a virtual agent-kind hook for the active
// instruction's agent. The runner-inner-child hands every agent-kind job to
// its HarnessAdapter directly, so a hook with no ScriptPath is dispatch-ready
// — see planner.PlanHook and adapters.HarnessAdapter.Run. The synthesis is
// gated to known harness agents (claude-code / codex / opencode) so unknown
// agent names do not collide with the shell adapter's Argv requirement.
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
	var agentHookDeclared bool
	for _, h := range hooks {
		if h.Kind == HandlerKindAgent {
			agentHookDeclared = true
		}
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

	if !agentHookDeclared {
		if syn := synthesizeAgentHook(task); syn != nil {
			matched = append(matched, *syn)
		}
	}
	return matched
}

// synthesizeAgentHook returns a virtual agent-kind hook for the task's
// active instruction, or nil if no synthesis applies. The fallback is gated
// on: (1) at least one instruction in the history, (2) the active
// instruction's agent name resolves to a known harness adapter via
// harnessTypeForAgent (anything that would fall through to "shell" is
// excluded — the shell adapter needs a real Argv it cannot produce here).
func synthesizeAgentHook(task *Task) *Hook {
	if task == nil || len(task.Instructions) == 0 {
		return nil
	}
	active := task.Instructions[len(task.Instructions)-1]
	if active.Agent == "" {
		return nil
	}
	if harnessTypeForAgent(active.Agent) == "shell" {
		return nil
	}
	return &Hook{
		ID:    "agent:" + active.Agent,
		Name:  active.Agent,
		Kind:  HandlerKindAgent,
		Agent: active.Agent,
	}
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

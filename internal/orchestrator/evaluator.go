package orchestrator

type Evaluator struct{}

// InstructionTypeForStatus maps a task status to the corresponding InstructionType.
func InstructionTypeForStatus(status TaskStatus) InstructionType {
	switch status {
	case TaskStatusExecuting:
		return InstructionTypeExecution
	case TaskStatusReworking:
		return InstructionTypeRework
	case TaskStatusVerifying:
		return InstructionTypeVerification
	default:
		return ""
	}
}

// extractInstructionConsumers returns the set of consumer names that have an
// instruction of the given type in the task.
func extractInstructionConsumers(instructions map[string]Instruction, instType InstructionType) map[string]bool {
	if instType == "" || len(instructions) == 0 {
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

// consumesInstructions reports whether the handler traits include the
// instructions trait. A hook declaring `consumes: [instructions]` opts into
// instructions routing.
//
// NOTE: Phase B 一時措置。Phase D で `consumes: [instructions]` 宣言を廃止し、
// routing 対象の別マーカーへ置き換える予定。
func consumesInstructions(traits HandlerTraits) bool {
	for _, t := range traits.Consumes {
		if t.Base() == TraitInstructions {
			return true
		}
	}
	return false
}

// Evaluate returns hooks that should fire for the given task.
// Hooks declaring `consumes: [instructions]` participate in instructions
// routing: they fire only when task.Instructions contains an instruction of
// the current status's type addressed to that hook's Consumer.
func (e *Evaluator) Evaluate(task *Task, hooks []Hook) []Hook {
	activeTraits, _ := ActiveTraitTypes(task.Payload)
	traitSet := make(map[TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	instType := InstructionTypeForStatus(task.Status)
	consumers := extractInstructionConsumers(task.Instructions, instType)

	var matched []Hook
	for _, h := range hooks {
		if !h.On.Contains(string(task.Status)) {
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

// EvaluateGates returns gates that should fire for the given task and phase.
// Unlike hooks, multiple gates may match the same state (kit composition).
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
		if !g.On.Contains(string(task.Status)) {
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
// TraitInstructions is ignored here because instructions moved out of payload
// into Task.Instructions (hook YAML still listing it as consumes is a legacy
// declaration that will be cleaned up in Phase D).
func hasAllTraits(set map[TraitType]bool, required []TraitType) bool {
	for _, t := range required {
		if t.IsOptional() {
			continue
		}
		if t.Base() == TraitInstructions {
			continue
		}
		if !set[t] {
			return false
		}
	}
	return true
}

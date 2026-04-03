package orchestrator

type Evaluator struct{}

// Evaluate returns hooks that should fire for the given task.
func (e *Evaluator) Evaluate(task *Task, hooks []Hook) []Hook {
	activeTraits, _ := ActiveTraitTypes(task.Payload)
	traitSet := make(map[TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []Hook
	for _, h := range hooks {
		if h.On != string(task.Status) {
			continue
		}
		if !hasAllTraits(traitSet, h.Traits.Consumes) {
			continue
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
		if g.On != string(task.Status) {
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
		if !set[t] {
			return false
		}
	}
	return true
}

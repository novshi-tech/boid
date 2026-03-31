package orchestrator

import (
	"github.com/novshi-tech/boid/internal/model"
)

type Evaluator struct{}

// Evaluate returns hooks that should fire for the given task.
func (e *Evaluator) Evaluate(task *model.Task, hooks []model.Hook) []model.Hook {
	activeTraits, _ := model.ActiveTraitTypes(task.Payload)
	traitSet := make(map[model.TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []model.Hook
	for _, h := range hooks {
		if h.On != string(task.Status) {
			continue
		}
		if !hasAllTraits(traitSet, h.RequiresTraits) {
			continue
		}
		matched = append(matched, h)
	}
	return matched
}

// EvaluateGates returns gates that should fire for the given task.
// Unlike hooks, multiple gates may match the same state (kit composition).
func (e *Evaluator) EvaluateGates(task *model.Task, gates []model.Gate) []model.Gate {
	activeTraits, _ := model.ActiveTraitTypes(task.Payload)
	traitSet := make(map[model.TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []model.Gate
	for _, g := range gates {
		if g.On != string(task.Status) {
			continue
		}
		if !hasAllTraits(traitSet, g.RequiresTraits) {
			continue
		}
		matched = append(matched, g)
	}
	return matched
}

func hasAllTraits(set map[model.TraitType]bool, required []model.TraitType) bool {
	for _, t := range required {
		if !set[t] {
			return false
		}
	}
	return true
}

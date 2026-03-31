package orchestrator

import (
	"github.com/novshi-tech/boid/internal/project"
)

type Evaluator struct{}

// Evaluate returns hooks that should fire for the given task.
func (e *Evaluator) Evaluate(task *Task, hooks []project.Hook) []project.Hook {
	activeTraits, _ := project.ActiveTraitTypes(task.Payload)
	traitSet := make(map[project.TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []project.Hook
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
func (e *Evaluator) EvaluateGates(task *Task, gates []project.Gate) []project.Gate {
	activeTraits, _ := project.ActiveTraitTypes(task.Payload)
	traitSet := make(map[project.TraitType]bool, len(activeTraits))
	for _, t := range activeTraits {
		traitSet[t] = true
	}

	var matched []project.Gate
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

func hasAllTraits(set map[project.TraitType]bool, required []project.TraitType) bool {
	for _, t := range required {
		if !set[t] {
			return false
		}
	}
	return true
}

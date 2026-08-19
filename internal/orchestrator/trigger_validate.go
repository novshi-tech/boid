package orchestrator

import (
	"fmt"
	"time"
)

// ValidateTriggers checks a project.yaml `triggers[]` list at LOAD time
// (called from parseProjectMetaBytes, spec_loader.go) — the same place
// validateHookKind validates task_behaviors.*.hooks. A malformed trigger
// must fail `boid project add`/`boid project fetch` loudly; the trigger
// sweep loop (internal/api/trigger_loop.go) trusts that every Trigger it
// reads off a hydrated ProjectMeta already passed this check.
func ValidateTriggers(triggers []Trigger) error {
	seen := make(map[string]bool, len(triggers))
	for i, trig := range triggers {
		if trig.Name == "" {
			return fmt.Errorf("project.yaml: triggers[%d]: name must not be empty", i)
		}
		if seen[trig.Name] {
			return fmt.Errorf("project.yaml: triggers[%d]: duplicate trigger name %q", i, trig.Name)
		}
		seen[trig.Name] = true
		if trig.Run == "" {
			return fmt.Errorf("project.yaml: triggers[%d] (%s): run must not be empty", i, trig.Name)
		}
		if trig.Every == "" {
			return fmt.Errorf("project.yaml: triggers[%d] (%s): every must not be empty", i, trig.Name)
		}
		every, err := time.ParseDuration(trig.Every)
		if err != nil {
			return fmt.Errorf("project.yaml: triggers[%d] (%s): every: %w", i, trig.Name, err)
		}
		if every <= 0 {
			return fmt.Errorf("project.yaml: triggers[%d] (%s): every must be > 0, got %q", i, trig.Name, trig.Every)
		}
	}
	return nil
}

package orchestrator

import (
	"fmt"
	"time"
)

// TriggerSweepResolution is the effective lower bound on `every`: the
// daemon only ever evaluates triggerIsDue once per TriggerLoop sweep tick
// (internal/server/wire.go wires TriggerLoop.Interval to this SAME
// constant, so they cannot drift apart), so an `every` below this value
// does not mean "run more often than once per sweep tick" — it silently
// collapses to "once per sweep tick", which is not what a project.yaml
// author who wrote e.g. `every: 1s` would expect (1 sweep tick = 1 minute
// here, not 1 second — ~1,440 containers/day, not ~86,400).
// ValidateTriggers rejects anything below this at project.yaml load time
// rather than let that surprise happen silently at runtime.
const TriggerSweepResolution = time.Minute

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
		switch trig.On {
		case "", TriggerOnSchedule, TriggerOnSignals:
			// ok — "" is an alias for TriggerOnSchedule (fields added after
			// this one existed, pre-dating `on`, round-trip unchanged).
		default:
			return fmt.Errorf("project.yaml: triggers[%d] (%s): on: unknown value %q (must be %q or %q)", i, trig.Name, trig.On, TriggerOnSchedule, TriggerOnSignals)
		}
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
		if trig.Timeout != "" {
			timeout, terr := time.ParseDuration(trig.Timeout)
			if terr != nil {
				return fmt.Errorf("project.yaml: triggers[%d] (%s): timeout: %w", i, trig.Name, terr)
			}
			if timeout <= 0 {
				return fmt.Errorf("project.yaml: triggers[%d] (%s): timeout must be > 0, got %q (omit the field for no bound)", i, trig.Name, trig.Timeout)
			}
			// Deliberately NOT rejected when timeout < every. `every: 1h,
			// timeout: 10m` ("look hourly; kill any round that runs past ten
			// minutes") is a coherent thing to ask for, and a round finishing
			// before the next one is due is the normal case, not a mistake.
		}
		if every < TriggerSweepResolution {
			return fmt.Errorf("project.yaml: triggers[%d] (%s): every (%q) is below the daemon's effective sweep resolution (%s) — it would silently run only once per sweep tick, not as often as written", i, trig.Name, trig.Every, TriggerSweepResolution)
		}
	}
	return nil
}

// TriggerTimeout resolves Timeout for the sweep loop. Zero means unbounded —
// both for an omitted field and for an unparseable one, which ValidateTriggers
// rejects at load time so it cannot reach a running daemon. Falling back to
// "unbounded" rather than to some default keeps a malformed value from
// inventing a bound nobody wrote.
func (t Trigger) TriggerTimeout() time.Duration {
	if t.Timeout == "" {
		return 0
	}
	d, err := time.ParseDuration(t.Timeout)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

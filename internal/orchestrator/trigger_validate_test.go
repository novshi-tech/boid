package orchestrator

import "testing"

// docs/plans/ingestion-identity.md PR-4 (B-5): ValidateTriggers is called
// from parseProjectMetaBytes (spec_loader.go) at project.yaml load time —
// the same place validateHookKind runs — so a malformed `triggers:` entry
// fails `boid project add`/`boid project fetch` loudly, rather than being
// silently skipped forever by the trigger sweep loop at runtime.

func TestValidateTriggers_Empty_OK(t *testing.T) {
	if err := ValidateTriggers(nil); err != nil {
		t.Errorf("ValidateTriggers(nil) = %v, want nil", err)
	}
	if err := ValidateTriggers([]Trigger{}); err != nil {
		t.Errorf("ValidateTriggers([]) = %v, want nil", err)
	}
}

func TestValidateTriggers_Valid_OK(t *testing.T) {
	err := ValidateTriggers([]Trigger{
		{Name: "intake", Every: "10m", Run: "python3 scripts/intake_tick.py"},
		{Name: "sweep", Every: "1h", Run: "bash scripts/sweep_tick.sh"},
	})
	if err != nil {
		t.Errorf("ValidateTriggers() = %v, want nil", err)
	}
}

func TestValidateTriggers_EmptyName_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "", Every: "10m", Run: "true"}})
	if err == nil {
		t.Fatal("ValidateTriggers() = nil, want an error for an empty name")
	}
}

func TestValidateTriggers_EmptyRun_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "10m", Run: ""}})
	if err == nil {
		t.Fatal("ValidateTriggers() = nil, want an error for an empty run")
	}
}

func TestValidateTriggers_EmptyEvery_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "", Run: "true"}})
	if err == nil {
		t.Fatal("ValidateTriggers() = nil, want an error for an empty every")
	}
}

func TestValidateTriggers_MalformedEvery_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "10 minutes", Run: "true"}})
	if err == nil {
		t.Fatal("ValidateTriggers() = nil, want an error for a non-time.ParseDuration every")
	}
}

func TestValidateTriggers_ZeroOrNegativeEvery_Rejected(t *testing.T) {
	for _, every := range []string{"0s", "0m", "-10m"} {
		if err := ValidateTriggers([]Trigger{{Name: "intake", Every: every, Run: "true"}}); err == nil {
			t.Errorf("ValidateTriggers(every=%q) = nil, want an error (must be > 0)", every)
		}
	}
}

// TestValidateTriggers_BelowSweepResolution_Rejected pins N-6 (Opus
// review): `every` has an effective floor of TriggerSweepResolution (the
// TriggerLoop's own sweep Interval, wire.go) — a sub-resolution `every`
// (e.g. "1s") does NOT mean "once a second", it silently means "once per
// sweep tick" instead, since triggerIsDue is only ever evaluated once per
// tick. ValidateTriggers rejects it loudly at project.yaml load time rather
// than let that surprise happen silently at runtime.
func TestValidateTriggers_BelowSweepResolution_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "1s", Run: "true"}})
	if err == nil {
		t.Fatal("ValidateTriggers(every=1s) = nil, want an error (below TriggerSweepResolution)")
	}
}

// TestValidateTriggers_AtSweepResolution_OK pins the floor's boundary: the
// resolution value itself (not just values below it) is accepted.
func TestValidateTriggers_AtSweepResolution_OK(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: TriggerSweepResolution.String(), Run: "true"}})
	if err != nil {
		t.Errorf("ValidateTriggers(every=%s) = %v, want nil (exactly at the floor)", TriggerSweepResolution, err)
	}
}

func TestValidateTriggers_DuplicateName_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{
		{Name: "intake", Every: "10m", Run: "true"},
		{Name: "intake", Every: "1h", Run: "false"},
	})
	if err == nil {
		t.Fatal("ValidateTriggers() = nil, want an error for a duplicate trigger name")
	}
}

// docs/plans/signal-ingest-detailed-design.md §4.1 (PR-6): `on` selects the
// trigger's activation predicate. Empty and "schedule" are equivalent (the
// pre-existing, unchanged behavior); "signals" additionally requires the
// project's workspace to have a pending Signal (SweepTriggers' due
// predicate, internal/api/trigger_loop.go). Anything else is a
// project.yaml authoring mistake and must fail loudly at load time, same as
// every other Trigger field ValidateTriggers checks.

func TestValidateTriggers_OnEmpty_OK(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "10m", Run: "true", On: ""}})
	if err != nil {
		t.Errorf("ValidateTriggers(on=\"\") = %v, want nil", err)
	}
}

func TestValidateTriggers_OnSchedule_OK(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "10m", Run: "true", On: "schedule"}})
	if err != nil {
		t.Errorf("ValidateTriggers(on=schedule) = %v, want nil", err)
	}
}

func TestValidateTriggers_OnSignals_OK(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "2m", Run: "python3 -m khi.app.scan", On: "signals"}})
	if err != nil {
		t.Errorf("ValidateTriggers(on=signals) = %v, want nil", err)
	}
}

func TestValidateTriggers_OnUnknown_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "intake", Every: "10m", Run: "true", On: "webhook"}})
	if err == nil {
		t.Fatal("ValidateTriggers(on=webhook) = nil, want an error for an unknown `on` value")
	}
}

// TestValidateTriggers_OnSignals_EveryStillRequired pins §4.1's "every は
// signals でも必須 — 発火間隔の下限 = debounce": on:signals does not relax the
// every-required / sweep-resolution-floor checks that apply to every trigger.
func TestValidateTriggers_OnSignals_EveryStillRequired(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "sweep", Run: "true", On: "signals"}})
	if err == nil {
		t.Fatal("ValidateTriggers(on=signals, every=\"\") = nil, want an error (every still required)")
	}
}

func TestValidateTriggers_OnSignals_BelowSweepResolution_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "1s", Run: "true", On: "signals"}})
	if err == nil {
		t.Fatal("ValidateTriggers(on=signals, every=1s) = nil, want an error (below TriggerSweepResolution)")
	}
}

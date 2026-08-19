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

func TestValidateTriggers_DuplicateName_Rejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{
		{Name: "intake", Every: "10m", Run: "true"},
		{Name: "intake", Every: "1h", Run: "false"},
	})
	if err == nil {
		t.Fatal("ValidateTriggers() = nil, want an error for a duplicate trigger name")
	}
}

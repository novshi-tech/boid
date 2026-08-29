package orchestrator

import (
	"strings"
	"testing"
	"time"
)

// timeout is optional — every trigger written before the field existed must
// still load, and its zero value must read as "no daemon-side bound".
func TestValidateTriggers_TimeoutIsOptional(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "2m", Run: "echo hi"}})
	if err != nil {
		t.Fatalf("a trigger with no timeout must be valid: %v", err)
	}
}

func TestValidateTriggers_TimeoutParses(t *testing.T) {
	if err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "echo hi"}}); err != nil {
		t.Fatalf("ValidateTriggers: %v", err)
	}
}

func TestValidateTriggers_TimeoutMustParse(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "2m", Timeout: "half an hour", Run: "echo hi"}})
	if err == nil {
		t.Fatal("an unparseable timeout must be rejected at load time")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want it to name the field", err)
	}
}

func TestValidateTriggers_TimeoutMustBePositive(t *testing.T) {
	if err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "2m", Timeout: "0s", Run: "echo hi"}}); err == nil {
		t.Fatal("a zero timeout written explicitly must be rejected — omit the field instead")
	}
	if err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "2m", Timeout: "-5m", Run: "echo hi"}}); err == nil {
		t.Fatal("a negative timeout must be rejected")
	}
}

// A timeout shorter than `every` describes a round that is killed before the
// next one may even start. That is always a mistake — the two are independent
// numbers (how often to check vs. how long a round may take), and the daemon
// cannot tell an author who mixed them up from one who meant it, so it says so
// at load time rather than silently killing every round.
func TestValidateTriggers_TimeoutBelowEveryIsRejected(t *testing.T) {
	err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "10m", Timeout: "2m", Run: "echo hi"}})
	if err == nil {
		t.Fatal("a timeout shorter than every must be rejected")
	}
	if !strings.Contains(err.Error(), "every") {
		t.Errorf("err = %v, want it to explain the relationship to every", err)
	}
}

// TriggerTimeout resolves the field for the sweep loop: zero means unbounded.
func TestTriggerTimeout(t *testing.T) {
	if got := (Trigger{}).TriggerTimeout(); got != 0 {
		t.Errorf("no timeout = %v, want 0 (unbounded)", got)
	}
	if got := (Trigger{Timeout: "30m"}).TriggerTimeout(); got != 30*time.Minute {
		t.Errorf("timeout = %v, want 30m", got)
	}
	// Unparseable cannot reach here (ValidateTriggers rejects it at load), but
	// resolving must not panic or invent a bound if it somehow does.
	if got := (Trigger{Timeout: "nonsense"}).TriggerTimeout(); got != 0 {
		t.Errorf("unparseable timeout = %v, want 0 (unbounded, not a made-up bound)", got)
	}
}

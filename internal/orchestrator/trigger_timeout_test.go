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

// A timeout SHORTER than `every` is accepted. `every: 1h, timeout: 10m` ("look
// hourly; kill any round that runs past ten minutes") is a coherent thing to
// ask for, and a round finishing before the next is due is the normal case.
//
// An earlier draft rejected it, on the stated grounds that confusing the two
// fields was the likeliest cause. The real reason was mechanical: the sweep
// checked the bound AFTER its `every`-due gate, so enforcement fired at
// max(every, timeout) and the constraint hid that. The check now runs before
// the gate, so there is nothing left to paper over — and this test exists to
// stop the constraint being reintroduced on the plausible-sounding rationale.
func TestValidateTriggers_TimeoutBelowEveryIsAccepted(t *testing.T) {
	if err := ValidateTriggers([]Trigger{{Name: "sweep", Every: "1h", Timeout: "10m", Run: "echo hi"}}); err != nil {
		t.Fatalf("timeout shorter than every must be accepted: %v", err)
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

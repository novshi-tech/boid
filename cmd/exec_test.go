package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
)

// TestPollExecExitCode_GivesUpWithSentinelWhenJobNeverReachesTerminalStatus
// is the Opus review finding #5 regression guard (PR #735): the give-up
// path must never report job.ExitCode's zero value as if it were a genuine
// exit 0 — that would be a false success. It must report the explicit
// execExitCodeUnknown sentinel instead (fail-safe: unknown ⇒ failure).
func TestPollExecExitCode_GivesUpWithSentinelWhenJobNeverReachesTerminalStatus(t *testing.T) {
	fetch := func() (api.Job, error) {
		return api.Job{Status: api.JobStatusRunning, ExitCode: 0}, nil
	}
	var sleeps int
	sleep := func(time.Duration) { sleeps++ }

	code, err := pollExecExitCode("job-1", fetch, sleep)
	if err != nil {
		t.Fatalf("pollExecExitCode: %v", err)
	}
	if code == 0 {
		t.Fatal("give-up path reported exit code 0 — a false success")
	}
	if code != execExitCodeUnknown {
		t.Errorf("code = %d, want sentinel %d", code, execExitCodeUnknown)
	}
	if sleeps == 0 {
		t.Error("expected pollExecExitCode to sleep between polls before giving up")
	}
}

// TestPollExecExitCode_ReturnsRealExitCodeOnceTerminal verifies the common
// path is unaffected by the give-up sentinel change: a job that reaches a
// terminal status mid-poll reports its real (possibly non-zero) ExitCode.
func TestPollExecExitCode_ReturnsRealExitCodeOnceTerminal(t *testing.T) {
	calls := 0
	fetch := func() (api.Job, error) {
		calls++
		if calls < 3 {
			return api.Job{Status: api.JobStatusRunning}, nil
		}
		return api.Job{Status: api.JobStatusFailed, ExitCode: 42}, nil
	}
	var sleeps int
	sleep := func(time.Duration) { sleeps++ }

	code, err := pollExecExitCode("job-1", fetch, sleep)
	if err != nil {
		t.Fatalf("pollExecExitCode: %v", err)
	}
	if code != 42 {
		t.Errorf("code = %d, want 42", code)
	}
	if sleeps != 2 {
		t.Errorf("sleeps = %d, want 2 (one per non-terminal poll before the terminal one)", sleeps)
	}
}

// TestPollExecExitCode_SuccessNotMistakenForGiveUp guards the zero-exit
// success case specifically: a job that completes (status=completed,
// exit_code=0) on the very first poll must return 0 with no error — the
// give-up sentinel must never shadow a real, promptly-observed success.
func TestPollExecExitCode_SuccessNotMistakenForGiveUp(t *testing.T) {
	fetch := func() (api.Job, error) {
		return api.Job{Status: api.JobStatusCompleted, ExitCode: 0}, nil
	}
	code, err := pollExecExitCode("job-1", fetch, func(time.Duration) {
		t.Fatal("should not sleep when the first poll is already terminal")
	})
	if err != nil {
		t.Fatalf("pollExecExitCode: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
}

// TestPollExecExitCode_PropagatesFetchError verifies a transport-level GET
// failure is surfaced immediately (unchanged behavior) rather than retried
// into the give-up path.
func TestPollExecExitCode_PropagatesFetchError(t *testing.T) {
	wantErr := errors.New("boom")
	fetch := func() (api.Job, error) { return api.Job{}, wantErr }
	code, err := pollExecExitCode("job-1", fetch, func(time.Duration) {
		t.Fatal("should not sleep on a fetch error")
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 on fetch error", code)
	}
}

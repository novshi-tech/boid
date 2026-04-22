package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
)

func makeTestGateJob(role string) *api.Job {
	job := makeTestJob(api.JobStatusCompleted)
	job.Role = role
	job.HandlerID = "gate-id-123"
	job.ExecutionState = "verifying"
	return job
}

// TestJobDetailR_NonGateJob_ShowsStatusMsg verifies R on a non-gate job shows an error message.
func TestJobDetailR_NonGateJob_ShowsStatusMsg(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	job.Role = "main"
	s := newTestJobDetailScreen(job)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})

	if s.statusMsg != "replay is only for gate jobs" {
		t.Errorf("expected statusMsg %q, got %q", "replay is only for gate jobs", s.statusMsg)
	}
	if s.replayPending {
		t.Error("replayPending should not be set for non-gate job")
	}
	if s.isError {
		t.Error("isError should be false for informational message")
	}
	if cmd == nil {
		t.Error("expected a clearStatus cmd")
	}
}

// TestJobDetailR_HookJob_ShowsStatusMsg verifies R on a hook job (not a gate) shows error message.
func TestJobDetailR_HookJob_ShowsStatusMsg(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	job.Role = "hook"
	s := newTestJobDetailScreen(job)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})

	if s.statusMsg != "replay is only for gate jobs" {
		t.Errorf("expected statusMsg %q, got %q", "replay is only for gate jobs", s.statusMsg)
	}
	if s.replayPending {
		t.Error("replayPending should not be set for hook job")
	}
}

// TestJobDetailR_GateJobNoExecutionState_ShowsUnavailableMsg verifies R on gate job with empty
// ExecutionState shows unavailable message.
func TestJobDetailR_GateJobNoExecutionState_ShowsUnavailableMsg(t *testing.T) {
	job := makeTestGateJob("exit_gate")
	job.ExecutionState = ""
	s := newTestJobDetailScreen(job)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})

	expected := "replay unavailable: legacy job has no execution_state"
	if s.statusMsg != expected {
		t.Errorf("expected statusMsg %q, got %q", expected, s.statusMsg)
	}
	if s.replayPending {
		t.Error("replayPending should not be set")
	}
	if cmd == nil {
		t.Error("expected a clearStatus cmd")
	}
}

// TestJobDetailR_GateJob_FirstPress_SetsPending verifies first R on valid gate job sets pending.
func TestJobDetailR_GateJob_FirstPress_SetsPending(t *testing.T) {
	for _, role := range []string{"gate", "entry_gate", "exit_gate"} {
		t.Run(role, func(t *testing.T) {
			job := makeTestGateJob(role)
			s := newTestJobDetailScreen(job)

			_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})

			if !s.replayPending {
				t.Error("replayPending should be set after first R")
			}
			if s.statusMsg != "Press R again to replay" {
				t.Errorf("expected %q, got %q", "Press R again to replay", s.statusMsg)
			}
			if s.isError {
				t.Error("isError should be false")
			}
			if cmd == nil {
				t.Error("expected a tick cmd for confirm deadline")
			}
		})
	}
}

// TestJobDetailR_GateJob_SecondPress_FiresReplayCmd verifies second R fires the replay cmd.
func TestJobDetailR_GateJob_SecondPress_FiresReplayCmd(t *testing.T) {
	job := makeTestGateJob("entry_gate")
	s := newTestJobDetailScreen(job)

	// First press: set pending
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})

	// Second press: fire replay
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})

	if s.replayPending {
		t.Error("replayPending should be reset after second R")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil cmd from second R press")
	}
	// The cmd will attempt to call the client; since shared.Client is nil in tests
	// we cannot actually invoke it. Just verify it produces a gateReplayResultMsg.
	// (calling cmd() would panic since Client is nil, so we skip that step)
}

// TestJobDetailReplay_ConfirmDeadline_ResetsPending verifies timeout resets pending state.
func TestJobDetailReplay_ConfirmDeadline_ResetsPending(t *testing.T) {
	job := makeTestGateJob("exit_gate")
	s := newTestJobDetailScreen(job)

	s.replayPending = true
	s.statusMsg = "Press R again to replay"

	s.Update(replayConfirmDeadlineMsg{})

	if s.replayPending {
		t.Error("replayPending should be reset on confirm deadline")
	}
	if s.statusMsg != "" {
		t.Errorf("statusMsg should be cleared on confirm deadline, got %q", s.statusMsg)
	}
}

// TestJobDetailGateReplayResult_Error_SetsErrorStatus verifies error result sets error status.
func TestJobDetailGateReplayResult_Error_SetsErrorStatus(t *testing.T) {
	job := makeTestGateJob("gate")
	s := newTestJobDetailScreen(job)
	s.replayPending = true

	_, cmd := s.Update(gateReplayResultMsg{err: fmt.Errorf("some replay error")})

	if !s.isError {
		t.Error("expected isError to be true on replay error")
	}
	if s.statusMsg == "" {
		t.Error("expected statusMsg to be set on replay error")
	}
	if s.replayPending {
		t.Error("replayPending should be reset on result")
	}
	if cmd == nil {
		t.Error("expected a clearStatus cmd")
	}
}

// TestJobDetailGateReplayResult_Success_PopsScreen verifies success result pops the screen.
func TestJobDetailGateReplayResult_Success_PopsScreen(t *testing.T) {
	job := makeTestGateJob("gate")
	s := newTestJobDetailScreen(job)
	s.replayPending = true

	_, cmd := s.Update(gateReplayResultMsg{result: &api.ReplayGateResult{}, err: nil})

	if s.replayPending {
		t.Error("replayPending should be reset on successful result")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil cmd after successful replay")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("expected popScreenMsg after successful replay, got %T", msg)
	}
}

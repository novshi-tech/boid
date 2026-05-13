package orchestrator

import (
	"strings"
	"testing"
)

func TestExpandTaskBaseBranch_NoVariable(t *testing.T) {
	got, err := ExpandTaskBaseBranch("main", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "main" {
		t.Errorf("got %q, want %q", got, "main")
	}
}

func TestExpandTaskBaseBranch_NoVariable_WithRemoteID(t *testing.T) {
	// Static value should pass through regardless of remoteID.
	got, err := ExpandTaskBaseBranch("main", "PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "main" {
		t.Errorf("got %q, want %q", got, "main")
	}
}

func TestExpandTaskBaseBranch_BracedVariable(t *testing.T) {
	got, err := ExpandTaskBaseBranch("feature/${TASK_REMOTE_ID}", "PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "feature/PROJ-123" {
		t.Errorf("got %q, want %q", got, "feature/PROJ-123")
	}
}

func TestExpandTaskBaseBranch_UnbracedVariable(t *testing.T) {
	got, err := ExpandTaskBaseBranch("feature/$TASK_REMOTE_ID", "PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "feature/PROJ-123" {
		t.Errorf("got %q, want %q", got, "feature/PROJ-123")
	}
}

func TestExpandTaskBaseBranch_EmptyRemoteID_Errors(t *testing.T) {
	_, err := ExpandTaskBaseBranch("feature/${TASK_REMOTE_ID}", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "TASK_REMOTE_ID") {
		t.Errorf("error message %q should mention TASK_REMOTE_ID", err.Error())
	}
}

func TestExpandTaskBaseBranch_UnknownVariable_Errors(t *testing.T) {
	_, err := ExpandTaskBaseBranch("feature/${UNKNOWN_VAR}", "PROJ-123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_VAR") {
		t.Errorf("error message %q should mention UNKNOWN_VAR", err.Error())
	}
}

func TestExpandTaskBaseBranch_CurrentBranchPreserved(t *testing.T) {
	// ${current_branch} is handled by ExpandBaseBranch, not ExpandTaskBaseBranch.
	// ExpandTaskBaseBranch should leave it untouched so the other expander can
	// process it. (The two expanders are composed in CreateTask.)
	got, err := ExpandTaskBaseBranch("${current_branch}", "PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "${current_branch}" {
		t.Errorf("got %q, want %q", got, "${current_branch}")
	}
}

func TestExpandTaskBaseBranch_MultipleOccurrences(t *testing.T) {
	got, err := ExpandTaskBaseBranch("${TASK_REMOTE_ID}/${TASK_REMOTE_ID}", "PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "PROJ-123/PROJ-123" {
		t.Errorf("got %q, want %q", got, "PROJ-123/PROJ-123")
	}
}

func TestExpandTaskBaseBranch_NoExpansionNeeded_EmptyRemoteIDOK(t *testing.T) {
	// Static base_branch with no template should not require remoteID.
	got, err := ExpandTaskBaseBranch("release/v1.0", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "release/v1.0" {
		t.Errorf("got %q, want %q", got, "release/v1.0")
	}
}

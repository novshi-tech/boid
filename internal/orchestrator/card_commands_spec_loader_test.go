package orchestrator_test

// card_commands / card_events are parsed off project.yaml's TOP LEVEL,
// through the ordinary ReadProjectMeta path — same placement as triggers[]
// (see trigger_spec_loader_test.go). The daemon never interprets a
// card_commands key's meaning; it only validates shape at load time.

import (
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// writeProjectYAML is defined in spec_loader_test.go (same package).

func TestReadProjectMeta_CardCommands_Valid(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  triage: {}
card_commands:
  discuss:
    label: Discuss
    run: python3 scripts/card_discuss.py
  review:
    label: Run
    run: python3 scripts/card_review.py
card_events:
  command: review
`)

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.CardCommands) != 2 {
		t.Fatalf("CardCommands = %+v, want 2 entries", meta.CardCommands)
	}
	discuss, ok := meta.CardCommands["discuss"]
	if !ok || discuss.Label != "Discuss" || discuss.Run != "python3 scripts/card_discuss.py" {
		t.Errorf("CardCommands[discuss] = %+v, unexpected", discuss)
	}
	review, ok := meta.CardCommands["review"]
	if !ok || review.Label != "Run" || review.Run != "python3 scripts/card_review.py" {
		t.Errorf("CardCommands[review] = %+v, unexpected", review)
	}
	if meta.CardEvents.Command != "review" {
		t.Errorf("CardEvents.Command = %q, want %q", meta.CardEvents.Command, "review")
	}
}

func TestReadProjectMeta_CardCommands_Absent_NilNotError(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.CardCommands) != 0 {
		t.Errorf("CardCommands = %+v, want empty when project.yaml has no card_commands: key", meta.CardCommands)
	}
	if meta.CardEvents.Command != "" {
		t.Errorf("CardEvents.Command = %q, want empty", meta.CardEvents.Command)
	}
}

func TestReadProjectMeta_CardCommands_MissingLabel_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  review:
    run: python3 scripts/card_review.py
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_commands entry missing label, got nil")
	}
}

func TestReadProjectMeta_CardCommands_MissingRun_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  review:
    label: Run
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_commands entry missing run, got nil")
	}
}

func TestReadProjectMeta_CardCommands_EmptyKey_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  "":
    label: Run
    run: python3 scripts/card_review.py
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_commands with empty key, got nil")
	}
}

func TestReadProjectMeta_CardCommands_WhitespaceOnlyLabel_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  review:
    label: "   "
    run: python3 scripts/card_review.py
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_commands entry with a whitespace-only label, got nil")
	}
}

func TestReadProjectMeta_CardCommands_WhitespaceOnlyRun_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  review:
    label: Run
    run: "   "
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_commands entry with a whitespace-only run, got nil")
	}
}

// TestReadProjectMeta_CardCommands_MultipleInvalidEntries_DeterministicError
// pins that ValidateCardCommands reports errors in a fixed (sorted-key)
// order rather than Go's randomized map iteration order — otherwise which
// entry's error surfaces would vary from run to run.
func TestReadProjectMeta_CardCommands_MultipleInvalidEntries_DeterministicError(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  zzz:
    run: python3 scripts/z.py
  aaa:
    run: python3 scripts/a.py
`)
	for i := 0; i < 20; i++ {
		_, err := projectspec.ReadProjectMeta(dir)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "card_commands.aaa") {
			t.Fatalf("run %d: expected error to name card_commands.aaa first (sorted order), got: %v", i, err)
		}
	}
}

// TestReadProjectMeta_CardEvents_UnknownCommand_RejectedAtLoadTime pins that
// card_events.command must reference a key actually declared in
// card_commands.
func TestReadProjectMeta_CardEvents_UnknownCommand_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  discuss:
    label: Discuss
    run: python3 scripts/card_discuss.py
card_events:
  command: review
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_events.command referencing an undeclared card_commands key, got nil")
	}
}

// TestReadProjectMeta_CardEvents_WithoutCardCommands_RejectedAtLoadTime pins
// that card_events.command cannot reference anything when card_commands
// itself is absent entirely.
func TestReadProjectMeta_CardEvents_WithoutCardCommands_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_events:
  command: review
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_events.command with no card_commands declared, got nil")
	}
}

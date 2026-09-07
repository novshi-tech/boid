package orchestrator_test

// card_commands / card_events are parsed off project.yaml's TOP LEVEL,
// through the ordinary ReadProjectMeta path — same placement as triggers[]
// (see trigger_spec_loader_test.go). The daemon never interprets a
// card_commands key's meaning; it only validates shape at load time.

import (
	"encoding/json"
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
    card_write: true
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
	if !ok || discuss.Label != "Discuss" || discuss.Run != "python3 scripts/card_discuss.py" || !discuss.CardWrite {
		t.Errorf("CardCommands[discuss] = %+v, unexpected", discuss)
	}
	review, ok := meta.CardCommands["review"]
	if !ok || review.Label != "Run" || review.Run != "python3 scripts/card_review.py" || review.CardWrite {
		t.Errorf("CardCommands[review] = %+v, want CardWrite=false (omitted in project.yaml)", review)
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

// TestReadProjectMeta_CardCommands_ReservedGoKey_RejectedAtLoadTime pins that
// a project.yaml cannot declare a card_commands key equal to
// CardRequestCommandKeyGo — that value is reserved to mark acceptGo's own
// card_requests reservation, and letting a project command collide with it
// would make ReconcileLaunchingCardRequests's Go skip predicate ambiguous.
func TestReadProjectMeta_CardCommands_ReservedGoKey_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  __go__:
    label: Run
    run: python3 scripts/card_review.py
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for card_commands key reserved for Go, got nil")
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

// TestReadProjectMeta_CardCommands_OrderPreserved pins that
// CardCommandsOrder carries project.yaml's declaration order, not Go map
// iteration order (which is randomized) and not sorted-key order (which
// ValidateCardCommands' error reporting uses internally but must not leak
// into the retained order). A regression back to a bare
// map[string]CardCommand for order-tracking purposes would make this test
// flaky/fail depending on map iteration.
func TestReadProjectMeta_CardCommands_OrderPreserved(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  zzz:
    label: ZZZ
    run: python3 scripts/zzz.py
  aaa:
    label: AAA
    run: python3 scripts/aaa.py
  mmm:
    label: MMM
    run: python3 scripts/mmm.py
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	want := []string{"zzz", "aaa", "mmm"}
	if len(meta.CardCommandsOrder) != len(want) {
		t.Fatalf("CardCommandsOrder = %v, want %v", meta.CardCommandsOrder, want)
	}
	for i, k := range want {
		if meta.CardCommandsOrder[i] != k {
			t.Fatalf("CardCommandsOrder = %v, want %v", meta.CardCommandsOrder, want)
		}
	}
}

// TestReadProjectMeta_CardCommands_Absent_OrderNilNotError is
// TestReadProjectMeta_CardCommands_Absent_NilNotError's counterpart for the
// order side channel.
func TestReadProjectMeta_CardCommands_Absent_OrderNilNotError(t *testing.T) {
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
	if len(meta.CardCommandsOrder) != 0 {
		t.Errorf("CardCommandsOrder = %v, want empty when project.yaml has no card_commands: key", meta.CardCommandsOrder)
	}
}

// TestReadProjectMeta_CardEvents_CommandTrimmed pins that stray whitespace
// around card_events.command (e.g. "review " from a hand-edited
// project.yaml) is trimmed before being checked against card_commands'
// keys, and the trimmed value is what callers read back.
func TestReadProjectMeta_CardEvents_CommandTrimmed(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  review:
    label: Run
    run: python3 scripts/card_review.py
card_events:
  command: "review "
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if meta.CardEvents.Command != "review" {
		t.Errorf("CardEvents.Command = %q, want %q (trimmed)", meta.CardEvents.Command, "review")
	}
}

// TestReadProjectMeta_CardEvents_TypoField_RejectedAtLoadTime pins that a
// typo'd field under card_events: (e.g. "commnad" instead of "command")
// fails load loudly instead of silently reading as unconfigured.
func TestReadProjectMeta_CardEvents_TypoField_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  review:
    label: Run
    run: python3 scripts/card_review.py
card_events:
  commnad: review
`)
	_, err := projectspec.ReadProjectMeta(dir)
	if err == nil {
		t.Fatal("expected error for a typo'd card_events field, got nil")
	}
}

// TestReadProjectMeta_CardEvents_KeyPresentButEmpty_LoadsFine pins that a
// `card_events:` key with no value under it (a null scalar node, e.g. left
// behind after commenting out `command:`) loads successfully with an empty
// CardEvents rather than failing — only a genuine mapping goes through the
// strict re-decode.
func TestReadProjectMeta_CardEvents_KeyPresentButEmpty_LoadsFine(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_events:
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if meta.CardEvents.Command != "" {
		t.Errorf("CardEvents.Command = %q, want empty", meta.CardEvents.Command)
	}
}

// TestProjectMeta_CardEvents_NotInJSON pins that CardEvents never appears
// in ProjectMeta's JSON output, even for a project with no card_commands.
func TestProjectMeta_CardEvents_NotInJSON(t *testing.T) {
	meta := projectspec.ProjectMeta{ID: "p", Name: "P"}
	out, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "card_events") {
		t.Errorf("JSON = %s, must not contain card_events", out)
	}
}

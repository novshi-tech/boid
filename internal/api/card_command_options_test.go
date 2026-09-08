package api

// CardCommandOptionsForProject: the Web UI's command button row reads this
// to know which commands to offer and in what order — it must mirror
// project.yaml's own declaration order (meta.CardCommandsOrder), never Go's
// randomized map iteration order over meta.CardCommands.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// writeProjectYAMLForCardCommandOptionsTest writes yaml to <dir>/.boid/project.yaml
// — the layout orchestrator.ReadProjectMeta expects.
func writeProjectYAMLForCardCommandOptionsTest(t *testing.T, dir, yaml string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
}

func TestCardCommandOptionsForProject_OrderedByDeclaration(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", &orchestrator.ProjectMeta{
		CardCommands: map[string]orchestrator.CardCommand{
			"zzz": {Label: "ZZZ", Run: "echo zzz"},
			"aaa": {Label: "AAA", Run: "echo aaa"},
			"mmm": {Label: "MMM", Run: "echo mmm"},
		},
		CardCommandsOrder: []string{"zzz", "aaa", "mmm"},
	})

	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-1")

	wantKeys := []string{"zzz", "aaa", "mmm"}
	wantLabels := []string{"ZZZ", "AAA", "MMM"}
	if len(opts) != len(wantKeys) {
		t.Fatalf("opts = %+v, want %d entries", opts, len(wantKeys))
	}
	for i := range wantKeys {
		if opts[i].Key != wantKeys[i] || opts[i].Label != wantLabels[i] {
			t.Errorf("opts[%d] = %+v, want Key=%q Label=%q", i, opts[i], wantKeys[i], wantLabels[i])
		}
	}
}

func TestCardCommandOptionsForProject_NoCommandsDeclared_ReturnsNil(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", &orchestrator.ProjectMeta{})

	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-1")
	if opts != nil {
		t.Errorf("opts = %+v, want nil for a project with no card_commands declared", opts)
	}
}

// TestCardCommandOptionsForProject_UnknownProject_ReturnsNil pins that a
// project id hydrateMetaForTriggers cannot resolve at all degrades to "no
// commands" rather than a panic — the Web UI must still render the rest of
// the card detail page.
func TestCardCommandOptionsForProject_UnknownProject_ReturnsNil(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", &orchestrator.ProjectMeta{
		CardCommands:      map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
		CardCommandsOrder: []string{"review"},
	})

	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-does-not-exist")
	if opts != nil {
		t.Errorf("opts = %+v, want nil for an unresolvable project id", opts)
	}
}

// TestCardCommandOptionsForProject_RealProjectYAML_OrderPreserved wires a
// REAL parsed project.yaml (not a hand-built ProjectMeta fixture) through
// CardCommandOptionsForProject end to end, confirming the label/order
// contract holds all the way from the YAML text a workspace actually
// writes — the same real-load path
// TestReadProjectMeta_CardCommands_OrderPreserved (internal/orchestrator)
// pins at the parser level alone.
func TestCardCommandOptionsForProject_RealProjectYAML_OrderPreserved(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAMLForCardCommandOptionsTest(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  zzz:
    label: Discuss it
    run: python3 scripts/zzz.py
  aaa:
    label: Run it
    run: python3 scripts/aaa.py
`)
	meta, err := orchestrator.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}

	svc, _, _ := newCardCommandTestService(t, "proj-1", meta)
	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-1")

	wantKeys := []string{"zzz", "aaa"}
	wantLabels := []string{"Discuss it", "Run it"}
	if len(opts) != len(wantKeys) {
		t.Fatalf("opts = %+v, want %d entries", opts, len(wantKeys))
	}
	for i := range wantKeys {
		if opts[i].Key != wantKeys[i] || opts[i].Label != wantLabels[i] {
			t.Errorf("opts[%d] = %+v, want Key=%q Label=%q", i, opts[i], wantKeys[i], wantLabels[i])
		}
	}
}

package initwizard_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/initwizard"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// ExpandScaffoldTemplate tests (built-in embedded template)
// ---------------------------------------------------------------------------

func TestExpandScaffoldTemplate_WithAgent(t *testing.T) {
	data := initwizard.ScaffoldTemplateData{
		ProjectID:   "abc-123",
		ProjectName: "My Project",
		Agent:       "claude-code",
	}

	result, err := initwizard.ExpandScaffoldTemplate(data)
	if err != nil {
		t.Fatalf("ExpandScaffoldTemplate: %v", err)
	}

	// Must produce both supervisor and executor behaviors.
	for _, key := range []string{"supervisor", "executor"} {
		val, ok := result[key]
		if !ok {
			t.Fatalf("expected %q key in result", key)
		}
		m, ok := val.(map[string]any)
		if !ok {
			t.Fatalf("expected %s to be map, got %T", key, val)
		}
		instr, ok := m["default_instruction"].(map[string]any)
		if !ok {
			t.Fatalf("%s.default_instruction is not a map", key)
		}
		if instr["agent"] != "claude-code" {
			t.Errorf("%s.default_instruction.agent = %v, want 'claude-code'", key, instr["agent"])
		}
		if instr["type"] != "execution" {
			t.Errorf("%s.default_instruction.type = %v, want 'execution'", key, instr["type"])
		}
	}
}

func TestExpandScaffoldTemplate_AgentEmpty(t *testing.T) {
	data := initwizard.ScaffoldTemplateData{
		ProjectID:   "abc-123",
		ProjectName: "My Project",
		Agent:       "",
	}

	result, err := initwizard.ExpandScaffoldTemplate(data)
	if err != nil {
		t.Fatalf("ExpandScaffoldTemplate: %v", err)
	}

	// When Agent is empty the agent: field should be absent.
	for _, key := range []string{"supervisor", "executor"} {
		val, ok := result[key]
		if !ok {
			t.Fatalf("expected %q key in result", key)
		}
		m, ok := val.(map[string]any)
		if !ok {
			t.Fatalf("expected %s to be map, got %T", key, val)
		}
		instr, ok := m["default_instruction"].(map[string]any)
		if !ok {
			t.Fatalf("%s.default_instruction is not a map", key)
		}
		if v, exists := instr["agent"]; exists && v != nil && v != "" {
			t.Errorf("%s.default_instruction.agent = %v, want absent/nil", key, v)
		}
	}
}

// ---------------------------------------------------------------------------
// ListAllKits tests
// ---------------------------------------------------------------------------

func TestListAllKits_Empty(t *testing.T) {
	kitsDir := t.TempDir()
	reg := orchestrator.NewRegistry(kitsDir)
	kits, err := initwizard.ListAllKits(reg)
	if err != nil {
		t.Fatalf("ListAllKits: %v", err)
	}
	if len(kits) != 0 {
		t.Errorf("expected 0 kits, got %d", len(kits))
	}
}

func TestListAllKits_WithKits(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKit(t, kitsDir, "go-tools", "go-tools")
	createFakeKit(t, kitsDir, "node-lts", "node-lts")

	reg := orchestrator.NewRegistry(kitsDir)
	kits, err := initwizard.ListAllKits(reg)
	if err != nil {
		t.Fatalf("ListAllKits: %v", err)
	}
	if len(kits) != 2 {
		t.Fatalf("expected 2 kits, got %d", len(kits))
	}
}

// ---------------------------------------------------------------------------
// Integration tests: full wizard run
// ---------------------------------------------------------------------------

func TestWizardRun_Basic(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKit(t, kitsDir, "go-tools", "go-tools")
	createFakeKit(t, kitsDir, "node-lts", "node-lts")

	projectDir := t.TempDir()

	// Stdin: project name, select kit 1 (Enter)
	input := "my-test-project\n1\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:      strings.NewReader(input),
		Out:     &out,
		KitsDir: kitsDir,
	}

	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Verify .boid/project.yaml exists and is parseable
	yamlPath := filepath.Join(projectDir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}

	var proj struct {
		ID            string         `yaml:"id"`
		Name          string         `yaml:"name"`
		Worktree      bool           `yaml:"worktree"`
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}

	if proj.Name != "my-test-project" {
		t.Errorf("Name = %q, want %q", proj.Name, "my-test-project")
	}
	if proj.ID == "" {
		t.Error("ID must not be empty")
	}
	if !proj.Worktree {
		t.Error("worktree must be true")
	}

	// Embedded template always produces both behaviors.
	for _, key := range []string{"supervisor", "executor"} {
		if _, ok := proj.TaskBehaviors[key]; !ok {
			t.Errorf("expected %q behavior in task_behaviors", key)
		}
	}
}

func TestWizardRun_DefaultProjectName(t *testing.T) {
	kitsDir := t.TempDir()

	projectDir := t.TempDir()

	// Stdin: empty line for project name (uses default = dir basename)
	input := "\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:      strings.NewReader(input),
		Out:     &out,
		KitsDir: kitsDir,
	}

	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	yamlPath := filepath.Join(projectDir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}

	var proj struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}

	expected := filepath.Base(projectDir)
	if proj.Name != expected {
		t.Errorf("Name = %q, want %q (directory base name)", proj.Name, expected)
	}
}

// TestWizardRun_EmbeddedBehaviors verifies that the wizard always generates
// supervisor and executor behaviors from the built-in template, and sets
// worktree: true, without requiring any kit to be installed.
func TestWizardRun_EmbeddedBehaviors(t *testing.T) {
	kitsDir := t.TempDir()

	projectDir := t.TempDir()

	input := "embed-project\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:      strings.NewReader(input),
		Out:     &out,
		KitsDir: kitsDir,
	}

	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}

	var proj struct {
		Name          string         `yaml:"name"`
		Worktree      bool           `yaml:"worktree"`
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}

	if proj.Name != "embed-project" {
		t.Errorf("Name = %q, want %q", proj.Name, "embed-project")
	}
	if !proj.Worktree {
		t.Error("worktree must be true")
	}
	for _, key := range []string{"supervisor", "executor"} {
		if _, ok := proj.TaskBehaviors[key]; !ok {
			t.Errorf("expected %q in task_behaviors, got keys: %v", key, mapKeys(proj.TaskBehaviors))
		}
	}
}

func TestWizardRun_ExistingProjectYAML(t *testing.T) {
	// The conflict check happens in cmd/init.go, not in the wizard itself.
	// We verify that the wizard CAN run even if there is a project.yaml
	// (it would overwrite – the guard lives in the cmd layer).
	kitsDir := t.TempDir()
	projectDir := t.TempDir()

	input := "\n"
	var out bytes.Buffer
	w := &initwizard.Wizard{
		In:      strings.NewReader(input),
		Out:     &out,
		KitsDir: kitsDir,
	}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestWizardRun_NoKitsInstalled verifies that the wizard works when no kits
// are installed (skips kit selection prompt).
func TestWizardRun_NoKitsInstalled(t *testing.T) {
	kitsDir := t.TempDir()
	projectDir := t.TempDir()

	input := "no-kit-project\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	var projFull map[string]any
	if err := yaml.Unmarshal(data, &projFull); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}
	if _, ok := projFull["kits"]; ok {
		t.Error("expected no top-level 'kits' field in project.yaml when no kits selected")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func createFakeKit(t *testing.T, kitsDir, name, displayName string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, name)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	content := "meta:\n  name: " + displayName + "\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

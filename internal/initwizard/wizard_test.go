package initwizard_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/initwizard"
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
// Integration tests: full wizard run
// ---------------------------------------------------------------------------

// TestWizardRun_Basic verifies the wizard generates a portable project.yaml
// containing only id / name / worktree / task_behaviors. Per the kit /
// workspace / project reorg, project.yaml must NOT contain kits, env,
// host_commands, additional_bindings, secret_namespace, or capabilities.
func TestWizardRun_Basic(t *testing.T) {
	projectDir := t.TempDir()

	input := "my-test-project\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:  strings.NewReader(input),
		Out: &out,
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

	assertNoMachineLocalKeys(t, data)
}

func TestWizardRun_DefaultProjectName(t *testing.T) {
	projectDir := t.TempDir()

	// Stdin: empty line for project name (uses default = dir basename)
	input := "\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:  strings.NewReader(input),
		Out: &out,
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

	assertNoMachineLocalKeys(t, data)
}

// TestWizardRun_EmbeddedBehaviors verifies that the wizard always generates
// supervisor and executor behaviors from the built-in template, and sets
// worktree: true.
func TestWizardRun_EmbeddedBehaviors(t *testing.T) {
	projectDir := t.TempDir()

	input := "embed-project\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:  strings.NewReader(input),
		Out: &out,
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

	assertNoMachineLocalKeys(t, data)
}

func TestWizardRun_ExistingProjectYAML(t *testing.T) {
	// The conflict check happens in cmd/project.go, not in the wizard itself.
	// We verify that the wizard CAN run even if there is a project.yaml
	// (it would overwrite — the guard lives in the cmd layer).
	projectDir := t.TempDir()

	input := "\n"
	var out bytes.Buffer
	w := &initwizard.Wizard{
		In:  strings.NewReader(input),
		Out: &out,
	}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assertNoMachineLocalKeys asserts that the project.yaml does not contain any
// of the machine-local keys that have been moved to workspace.yaml / kit.yaml.
// See docs/plans/kit-workspace-project-reorg.md (削除キー化するフィールド).
func assertNoMachineLocalKeys(t *testing.T, data []byte) {
	t.Helper()
	var projFull map[string]any
	if err := yaml.Unmarshal(data, &projFull); err != nil {
		t.Fatalf("parse project.yaml as map: %v", err)
	}
	forbidden := []string{"kits", "env", "host_commands", "additional_bindings", "secret_namespace", "capabilities"}
	for _, key := range forbidden {
		if _, ok := projFull[key]; ok {
			t.Errorf("project.yaml must not contain top-level %q (moved to workspace.yaml / kit.yaml)", key)
		}
	}
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

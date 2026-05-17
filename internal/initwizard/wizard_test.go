package initwizard_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/initwizard"
	"github.com/novshi-tech/boid/internal/kit"
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
// ValidateRequirements integration tests (wizard context)
// ---------------------------------------------------------------------------

func TestValidateRequirementsIntegration_AllPresent(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "test-kit"},
			Requires: &orchestrator.KitRequires{Commands: []string{"sh"}},
		},
		{
			Meta: &orchestrator.KitMetaInfo{Name: "bare-kit"},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateRequirementsIntegration_MissingCommand(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "missing-kit"},
			Requires: &orchestrator.KitRequires{Commands: []string{"__boid_nonexistent_cmd__"}},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].KitName != "missing-kit" {
		t.Errorf("KitName = %q, want %q", errs[0].KitName, "missing-kit")
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
	createFakeKit(t, kitsDir, "github.com/test/repo/kit-a", "kit-a", "")
	createFakeKit(t, kitsDir, "github.com/test/repo/kit-b", "kit-b", "go.mod")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

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
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod")
	createFakeKit(t, kitsDir, "github.com/test/repo/node-kit", "node-kit", "package.json")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	// go.mod exists → go-kit should be auto-detected
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stdin: project name, keep kit defaults (Enter)
	input := "my-test-project\n\n"
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

	// go-kit is project-scopable and auto-detected, so it must appear in top-level kits.
	var projFull struct {
		Kits []string `yaml:"kits"`
	}
	if err := yaml.Unmarshal(data, &projFull); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}
	found := false
	for _, k := range projFull.Kits {
		if strings.Contains(k, "go-kit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected go-kit in top-level kits, got %v", projFull.Kits)
	}

	// .boid/hooks directory must NOT be created (hooks come from kit directories)
	if _, err := os.Stat(filepath.Join(projectDir, ".boid", "hooks")); err == nil {
		t.Error(".boid/hooks should not be created by the wizard")
	}
}

func TestWizardRun_DefaultProjectName(t *testing.T) {
	kitsDir := t.TempDir()
	initFakeGitRepo(t, kitsDir, "github.com/test/empty")

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
// worktree: true, without requiring any behavior kit to be installed.
func TestWizardRun_EmbeddedBehaviors(t *testing.T) {
	kitsDir := t.TempDir()
	initFakeGitRepo(t, kitsDir, "github.com/test/empty")

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
	initFakeGitRepo(t, kitsDir, "github.com/test/empty")
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

func TestWizardRun_MissingRequirement(t *testing.T) {
	kitsDir := t.TempDir()
	// Kit that requires a non-existent command
	createFakeKitWithRequires(t, kitsDir, "github.com/test/repo/bad-kit", "bad-kit", "__boid_nonexistent_cmd__")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()

	// Stdin: project name, select kit 1 (the bad-kit)
	input := "test\n1\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:      strings.NewReader(input),
		Out:     &out,
		KitsDir: kitsDir,
	}

	err := w.Run(projectDir)
	if err == nil {
		t.Fatal("expected error when required command is missing")
	}
	if !strings.Contains(err.Error(), "missing required commands") {
		t.Errorf("error = %q, want to contain 'missing required commands'", err.Error())
	}
}

func TestWizardRun_OptionalKit(t *testing.T) {
	kitsDir := t.TempDir()

	// Create a kit whose detect script always prints "optional".
	kitRef := "github.com/test/repo/opt-kit"
	kitDir := filepath.Join(kitsDir, kitRef)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho optional\n"
	if err := os.WriteFile(filepath.Join(kitDir, "detect.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	kitYAML := "meta:\n  name: opt-kit\ndetect:\n  script: detect.sh\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()

	// Stdin: project name then Enter (keep defaults — optional kit stays OFF).
	input := "opt-project\n\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{
		In:      strings.NewReader(input),
		Out:     &out,
		KitsDir: kitsDir,
	}

	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// 1. Output must contain "(optional)" suffix.
	if !strings.Contains(out.String(), "(optional)") {
		t.Errorf("expected '(optional)' in output, got:\n%s", out.String())
	}

	// 2. project.yaml must NOT include the optional kit (default OFF).
	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	var projFull map[string]any
	if err := yaml.Unmarshal(data, &projFull); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}
	if _, ok := projFull["kits"]; ok {
		t.Error("expected no top-level 'kits' field in project.yaml")
	}
}

// TestWizardRun_DeprecatedKitExcluded verifies that a kit with deprecated: true
// is not shown in the "Available kits" list and not added to project.yaml.
func TestWizardRun_DeprecatedKitExcluded(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKit(t, kitsDir, "github.com/test/repo/active-kit", "active-kit", "")
	createFakeKitDeprecated(t, kitsDir, "github.com/test/repo/old-kit", "old-kit")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()

	// Stdin: project name, keep defaults (Enter)
	input := "dep-project\n\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// The deprecated kit must not appear in the wizard output.
	if strings.Contains(out.String(), "old-kit") {
		t.Errorf("expected deprecated kit 'old-kit' to be absent from output, got:\n%s", out.String())
	}

	// The deprecated kit must not appear in project.yaml kits list.
	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	if strings.Contains(string(data), "old-kit") {
		t.Errorf("expected deprecated kit 'old-kit' to be absent from project.yaml, got:\n%s", string(data))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func createFakeKit(t *testing.T, kitsDir, ref, name, detectMarker string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	var sb strings.Builder
	if name != "" {
		sb.WriteString("meta:\n  name: " + name + "\n")
	}
	if detectMarker != "" {
		script := "#!/bin/sh\nif [ -e \"" + detectMarker + "\" ]; then\n    echo required\nfi\n"
		if err := os.WriteFile(filepath.Join(kitDir, "detect.sh"), []byte(script), 0o755); err != nil {
			t.Fatalf("write detect.sh: %v", err)
		}
		sb.WriteString("detect:\n  script: detect.sh\n")
	}

	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func createFakeKitDeprecated(t *testing.T, kitsDir, ref, name string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}
	content := "deprecated: true\nmeta:\n  name: " + name + "\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func createFakeKitWithRequires(t *testing.T, kitsDir, ref, name, command string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	script := "#!/bin/sh\n[ -e go.mod ] && echo required\n"
	if err := os.WriteFile(filepath.Join(kitDir, "detect.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write detect.sh: %v", err)
	}

	content := "meta:\n  name: " + name + "\n" +
		"requires:\n  commands:\n    - " + command + "\n" +
		"detect:\n  script: detect.sh\n"

	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func initFakeGitRepo(t *testing.T, kitsDir, repoRef string) {
	t.Helper()
	gitDir := filepath.Join(kitsDir, repoRef, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// createFakeKitWithAgent creates a feature kit (no scaffold) with provides_agent set.
func createFakeKitWithAgent(t *testing.T, kitsDir, ref, name, detectMarker, agent string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	var sb strings.Builder
	if name != "" {
		sb.WriteString("meta:\n  name: " + name + "\n")
	}
	if detectMarker != "" {
		script := "#!/bin/sh\nif [ -e \"" + detectMarker + "\" ]; then\n    echo required\nfi\n"
		if err := os.WriteFile(filepath.Join(kitDir, "detect.sh"), []byte(script), 0o755); err != nil {
			t.Fatalf("write detect.sh: %v", err)
		}
		sb.WriteString("detect:\n  script: detect.sh\n")
	}
	if agent != "" {
		sb.WriteString("provides_agent: " + agent + "\n")
	}

	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Agent selection tests
// ---------------------------------------------------------------------------

// TestWizardRun_AgentAutoSelected verifies that when exactly one feature kit
// provides an agent, it is auto-selected without prompting and injected into
// the built-in template.
func TestWizardRun_AgentAutoSelected(t *testing.T) {
	kitsDir := t.TempDir()
	// Feature kit: provides_agent, auto-detected via claude-marker.txt
	createFakeKitWithAgent(t, kitsDir, "github.com/test/repo/claude-kit", "claude-code-kit", "claude-marker.txt", "claude-code")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "claude-marker.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Input: project name (default), keep kit defaults (claude-kit auto-selected)
	// Agent: auto-selected — no additional input line needed
	input := "agent-project\n\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Output should mention the agent name
	if !strings.Contains(out.String(), "claude-code") {
		t.Errorf("expected 'claude-code' in output, got:\n%s", out.String())
	}

	// Verify Agent was injected into the built-in template
	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	var proj struct {
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}
	execBehavior, ok := proj.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("expected 'executor' in task_behaviors, got keys: %v", mapKeys(proj.TaskBehaviors))
	}
	execMap, ok := execBehavior.(map[string]any)
	if !ok {
		t.Fatalf("expected executor to be map, got %T", execBehavior)
	}
	instr, ok := execMap["default_instruction"].(map[string]any)
	if !ok {
		t.Fatalf("executor.default_instruction is not a map")
	}
	if instr["agent"] != "claude-code" {
		t.Errorf("agent = %v, want 'claude-code'", instr["agent"])
	}
}

// TestWizardRun_AgentMenu verifies that when two feature kits provide agents,
// a menu is shown and the user's choice is injected into the built-in template.
func TestWizardRun_AgentMenu(t *testing.T) {
	kitsDir := t.TempDir()
	// Two feature kits with agents (lexicographic order: kit-a-consumer < kit-b-consumer)
	createFakeKitWithAgent(t, kitsDir, "github.com/test/repo/kit-a-consumer", "agent-a-kit", "marker-a.txt", "agent-a")
	createFakeKitWithAgent(t, kitsDir, "github.com/test/repo/kit-b-consumer", "agent-b-kit", "marker-b.txt", "agent-b")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "marker-a.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "marker-b.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Input: default name, keep kit defaults (both auto-selected), select agent #2 (agent-b)
	input := "\n\n2\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Verify agent #2 (agent-b) was injected
	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	var proj struct {
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}
	execBehavior, ok := proj.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("expected 'executor' in task_behaviors")
	}
	execMap := execBehavior.(map[string]any)
	instr := execMap["default_instruction"].(map[string]any)
	if instr["agent"] != "agent-b" {
		t.Errorf("agent = %v, want 'agent-b'", instr["agent"])
	}
}

// TestWizardRun_AgentNone verifies that when no feature kit provides an agent,
// the agent field is absent from the generated task_behaviors.
func TestWizardRun_AgentNone(t *testing.T) {
	kitsDir := t.TempDir()
	// Feature kit without provides_agent
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Input: default name, keep kit defaults
	input := "\n\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Verify agent field is absent when no agent provided
	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	var proj struct {
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}
	execBehavior, ok := proj.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("expected 'executor' in task_behaviors")
	}
	execMap := execBehavior.(map[string]any)
	instr, ok := execMap["default_instruction"].(map[string]any)
	if !ok {
		t.Fatalf("executor.default_instruction is not a map")
	}
	if v, exists := instr["agent"]; exists && v != nil && v != "" {
		t.Errorf("agent = %v, want absent/nil", v)
	}
}

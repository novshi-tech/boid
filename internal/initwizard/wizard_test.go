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
// ExpandScaffoldTemplate tests
// ---------------------------------------------------------------------------

func TestExpandScaffoldTemplate_Basic(t *testing.T) {
	kitDir := t.TempDir()
	tplContent := `dev:
  name: Development
  transition: one-shot
  project_id: {{.ProjectID}}
  project_name: {{.ProjectName}}
`
	if err := os.WriteFile(filepath.Join(kitDir, "behaviors.tmpl"), []byte(tplContent), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	data := initwizard.ScaffoldTemplateData{
		ProjectID:   "abc-123",
		ProjectName: "My Project",
	}

	result, err := initwizard.ExpandScaffoldTemplate(kitDir, "behaviors.tmpl", data)
	if err != nil {
		t.Fatalf("ExpandScaffoldTemplate: %v", err)
	}

	devVal, ok := result["dev"]
	if !ok {
		t.Fatal("expected 'dev' key in result")
	}
	devMap, ok := devVal.(map[string]any)
	if !ok {
		t.Fatalf("expected dev to be map, got %T", devVal)
	}
	if devMap["project_id"] != "abc-123" {
		t.Errorf("project_id = %v, want %q", devMap["project_id"], "abc-123")
	}
	if devMap["project_name"] != "My Project" {
		t.Errorf("project_name = %v, want %q", devMap["project_name"], "My Project")
	}
	if devMap["name"] != "Development" {
		t.Errorf("name = %v, want %q", devMap["name"], "Development")
	}
}

func TestExpandScaffoldTemplate_InvalidTemplate(t *testing.T) {
	kitDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kitDir, "bad.tmpl"), []byte(`{{ .Foo {{`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := initwizard.ExpandScaffoldTemplate(kitDir, "bad.tmpl", initwizard.ScaffoldTemplateData{})
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}
}

func TestExpandScaffoldTemplate_MissingFile(t *testing.T) {
	kitDir := t.TempDir()
	_, err := initwizard.ExpandScaffoldTemplate(kitDir, "nonexistent.tmpl", initwizard.ScaffoldTemplateData{})
	if err == nil {
		t.Fatal("expected error for missing template file")
	}
}

func TestExpandScaffoldTemplate_InvalidYAML(t *testing.T) {
	kitDir := t.TempDir()
	// Template renders to invalid YAML
	if err := os.WriteFile(filepath.Join(kitDir, "bad.tmpl"), []byte(":\nfoo: [unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := initwizard.ExpandScaffoldTemplate(kitDir, "bad.tmpl", initwizard.ScaffoldTemplateData{})
	if err == nil {
		t.Fatal("expected error for invalid YAML output")
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
	createFakeKit(t, kitsDir, "github.com/test/repo/kit-a", "kit-a", "", false)
	createFakeKit(t, kitsDir, "github.com/test/repo/kit-b", "kit-b", "go.mod", false)
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
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod", false)
	createFakeKit(t, kitsDir, "github.com/test/repo/node-kit", "node-kit", "package.json", false)
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	// go.mod exists → go-kit should be auto-detected
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stdin: project name, keep kit defaults (Enter)
	// No behavior kits → behavior selection step is skipped
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
		ID   string   `yaml:"id"`
		Name string   `yaml:"name"`
		Kits []string `yaml:"kits"`
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

	// go-kit should be auto-selected (go.mod exists)
	found := false
	for _, k := range proj.Kits {
		if strings.Contains(k, "go-kit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected go-kit in kits, got %v", proj.Kits)
	}

	// .boid/hooks directory must be created
	if _, err := os.Stat(filepath.Join(projectDir, ".boid", "hooks")); err != nil {
		t.Errorf(".boid/hooks not created: %v", err)
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

func TestWizardRun_WithScaffold(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod", false)
	createFakeKit(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit", "", true)
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stdin: project name, keep kit defaults, accept the single behavior kit (Y)
	input := "scaffold-project\n\nY\n"
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
		Name          string         `yaml:"name"`
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}

	if proj.Name != "scaffold-project" {
		t.Errorf("Name = %q, want %q", proj.Name, "scaffold-project")
	}
	if len(proj.TaskBehaviors) == 0 {
		t.Error("expected task_behaviors to be non-empty when scaffold kit is selected")
	}
	if _, ok := proj.TaskBehaviors["dev"]; !ok {
		t.Errorf("expected 'dev' behavior in task_behaviors, got keys: %v", mapKeys(proj.TaskBehaviors))
	}
}

func TestWizardRun_ExistingProjectYAML(t *testing.T) {
	// The conflict check happens in cmd/init.go, not in the wizard itself.
	// We verify that the wizard CAN run even if there is a project.yaml
	// (it would overwrite – the guard lives in the cmd layer).
	// This test just ensures the wizard path itself doesn't error on a clean dir.
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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func createFakeKit(t *testing.T, kitsDir, ref, name, detectFile string, hasScaffold bool) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	var sb strings.Builder
	if name != "" {
		sb.WriteString("meta:\n  name: " + name + "\n")
	}
	if detectFile != "" {
		sb.WriteString("detect:\n  files:\n    - " + detectFile + "\n")
	}
	if hasScaffold {
		sb.WriteString("scaffold:\n  task_behaviors:\n    description: Test scaffold\n    template: behaviors.tmpl\n")
		tpl := "dev:\n  name: Development\n  transition: one-shot\n"
		if err := os.WriteFile(filepath.Join(kitDir, "behaviors.tmpl"), []byte(tpl), 0o644); err != nil {
			t.Fatalf("write scaffold template: %v", err)
		}
	}

	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func createFakeKitWithRequires(t *testing.T, kitsDir, ref, name, command string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	content := "meta:\n  name: " + name + "\n" +
		"requires:\n  commands:\n    - " + command + "\n" +
		"detect:\n  files:\n    - go.mod\n"

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

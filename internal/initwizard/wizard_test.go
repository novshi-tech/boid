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

func TestExpandScaffoldTemplate_WithFeatureKits(t *testing.T) {
	kitDir := t.TempDir()
	tplContent := `dev:
  kits:
{{- range .FeatureKits}}
  - {{.}}
{{- end}}
`
	if err := os.WriteFile(filepath.Join(kitDir, "behaviors.tmpl"), []byte(tplContent), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	data := initwizard.ScaffoldTemplateData{
		ProjectID:   "abc-123",
		ProjectName: "My Project",
		FeatureKits: []string{"github.com/test/repo/go-kit", "github.com/test/repo/node-kit"},
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
	kits, ok := devMap["kits"].([]any)
	if !ok {
		t.Fatalf("expected dev.kits to be a list, got %T", devMap["kits"])
	}
	if len(kits) != 2 {
		t.Fatalf("expected 2 kits, got %d: %v", len(kits), kits)
	}
	if kits[0] != "github.com/test/repo/go-kit" {
		t.Errorf("kits[0] = %v, want %q", kits[0], "github.com/test/repo/go-kit")
	}
	if kits[1] != "github.com/test/repo/node-kit" {
		t.Errorf("kits[1] = %v, want %q", kits[1], "github.com/test/repo/node-kit")
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
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
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

	// Top-level kits field must not be present (kits are per-behavior via template)
	var projFull map[string]any
	if err := yaml.Unmarshal(data, &projFull); err != nil {
		t.Fatalf("parse project.yaml as map: %v", err)
	}
	if _, ok := projFull["kits"]; ok {
		t.Error("expected no top-level 'kits' field in project.yaml")
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
	devVal, ok := proj.TaskBehaviors["dev"]
	if !ok {
		t.Fatalf("expected 'dev' behavior in task_behaviors, got keys: %v", mapKeys(proj.TaskBehaviors))
	}
	devMap, ok := devVal.(map[string]any)
	if !ok {
		t.Fatalf("expected dev to be map, got %T", devVal)
	}
	kits, ok := devMap["kits"].([]any)
	if !ok {
		t.Fatalf("expected dev.kits to be a list, got %T", devMap["kits"])
	}
	found := false
	for _, k := range kits {
		if s, ok := k.(string); ok && strings.Contains(s, "go-kit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected go-kit in task_behaviors.dev.kits, got %v", kits)
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
	// Top-level kits field is not written; kits live inside task_behaviors via template.
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

// ---------------------------------------------------------------------------
// scaffold.commands tests
// ---------------------------------------------------------------------------

// createFakeKitWithCommands creates a behavior kit that declares both
// scaffold.task_behaviors and scaffold.commands templates.
func createFakeKitWithCommands(t *testing.T, kitsDir, ref, name string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	behaviorsTpl := "dev:\n  name: Development\n  consumer: {{.Consumer}}\n  kits:\n{{- range .FeatureKits}}\n  - {{.}}\n{{- end}}\n"
	if err := os.WriteFile(filepath.Join(kitDir, "behaviors.tmpl"), []byte(behaviorsTpl), 0o644); err != nil {
		t.Fatalf("write behaviors.tmpl: %v", err)
	}

	commandsTpl := "init:\n  consumer: {{.Consumer}}\n  feature_kits:\n{{- range .FeatureKits}}\n  - {{.}}\n{{- end}}\n"
	if err := os.WriteFile(filepath.Join(kitDir, "commands.tmpl"), []byte(commandsTpl), 0o644); err != nil {
		t.Fatalf("write commands.tmpl: %v", err)
	}

	kitYAML := "meta:\n  name: " + name + "\n" +
		"scaffold:\n" +
		"  task_behaviors:\n    description: Test scaffold\n    template: behaviors.tmpl\n" +
		"  commands:\n    description: Command templates\n    template: commands.tmpl\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

// TestWizardRun_WithScaffoldCommands verifies that when a behavior kit declares
// scaffold.commands, the generated project.yaml contains a commands: section.
func TestWizardRun_WithScaffoldCommands(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod", false)
	createFakeKitWithCommands(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stdin: project name, keep kit defaults, accept the single behavior kit (Y)
	input := "cmd-project\n\nY\n"
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
		TaskBehaviors map[string]any `yaml:"task_behaviors"`
		Commands      map[string]any `yaml:"commands"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}

	if proj.Name != "cmd-project" {
		t.Errorf("Name = %q, want %q", proj.Name, "cmd-project")
	}
	if len(proj.Commands) == 0 {
		t.Error("expected commands to be non-empty when scaffold.commands is declared")
	}
	initVal, ok := proj.Commands["init"]
	if !ok {
		t.Fatalf("expected 'init' key in commands, got keys: %v", mapKeys(proj.Commands))
	}
	initMap, ok := initVal.(map[string]any)
	if !ok {
		t.Fatalf("expected init to be map, got %T", initVal)
	}
	// feature_kits should contain go-kit ref
	fkRaw, ok := initMap["feature_kits"].([]any)
	if !ok {
		t.Fatalf("expected feature_kits to be a list, got %T", initMap["feature_kits"])
	}
	found := false
	for _, k := range fkRaw {
		if s, ok := k.(string); ok && strings.Contains(s, "go-kit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected go-kit in commands.init.feature_kits, got %v", fkRaw)
	}
}

// TestWizardRun_NoScaffoldCommands verifies that when scaffold.commands is absent,
// the commands: key is omitted from project.yaml (omitempty).
func TestWizardRun_NoScaffoldCommands(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod", false)
	// hasScaffold=true but only task_behaviors, no commands
	createFakeKit(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit", "", true)
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := "no-cmd-project\n\nY\n"
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

	var projFull map[string]any
	if err := yaml.Unmarshal(data, &projFull); err != nil {
		t.Fatalf("parse project.yaml as map: %v", err)
	}
	if _, ok := projFull["commands"]; ok {
		t.Error("expected no 'commands' key in project.yaml when scaffold.commands is not declared")
	}
}

// TestWizardRun_ScaffoldCommandsConsumerFeatureKits verifies that .Consumer and
// .FeatureKits are correctly expanded in the scaffold.commands template.
func TestWizardRun_ScaffoldCommandsConsumerFeatureKits(t *testing.T) {
	kitsDir := t.TempDir()
	createFakeKitWithConsumer(t, kitsDir, "github.com/test/repo/claude-kit", "claude-code-kit", "claude-marker.txt", "claude-code")
	createFakeKitWithCommands(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "claude-marker.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Consumer auto-selected (only one), accept behavior kit (Y)
	input := "consumer-cmd-project\n\nY\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}

	var proj struct {
		Commands map[string]any `yaml:"commands"`
	}
	if err := yaml.Unmarshal(data, &proj); err != nil {
		t.Fatalf("parse project.yaml: %v", err)
	}

	initVal, ok := proj.Commands["init"]
	if !ok {
		t.Fatalf("expected 'init' key in commands")
	}
	initMap := initVal.(map[string]any)
	if initMap["consumer"] != "claude-code" {
		t.Errorf("commands.init.consumer = %v, want 'claude-code'", initMap["consumer"])
	}
	fkRaw, ok := initMap["feature_kits"].([]any)
	if !ok {
		t.Fatalf("expected feature_kits to be a list, got %T", initMap["feature_kits"])
	}
	found := false
	for _, k := range fkRaw {
		if s, ok := k.(string); ok && strings.Contains(s, "claude-kit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected claude-kit in commands.init.feature_kits, got %v", fkRaw)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func createFakeKit(t *testing.T, kitsDir, ref, name, detectMarker string, hasScaffold bool) {
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
	if hasScaffold {
		sb.WriteString("scaffold:\n  task_behaviors:\n    description: Test scaffold\n    template: behaviors.tmpl\n")
		tpl := "dev:\n  name: Development\n  kits:\n{{- range .FeatureKits}}\n  - {{.}}\n{{- end}}\n"
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

// createFakeKitWithConsumer creates a feature kit (no scaffold) with provides_consumer set.
func createFakeKitWithConsumer(t *testing.T, kitsDir, ref, name, detectMarker, consumer string) {
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
	if consumer != "" {
		sb.WriteString("provides_consumer: " + consumer + "\n")
	}

	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

// createFakeScaffoldKitConsumerTemplate creates a behavior kit whose scaffold template
// uses {{.Consumer}}, so tests can verify Consumer injection.
func createFakeScaffoldKitConsumerTemplate(t *testing.T, kitsDir, ref, name string) {
	t.Helper()
	kitDir := filepath.Join(kitsDir, ref)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}
	tpl := "dev:\n  consumer: {{.Consumer}}\n"
	if err := os.WriteFile(filepath.Join(kitDir, "behaviors.tmpl"), []byte(tpl), 0o644); err != nil {
		t.Fatalf("write behaviors.tmpl: %v", err)
	}
	kitYAML := "meta:\n  name: " + name + "\n" +
		"scaffold:\n  task_behaviors:\n    description: Test scaffold\n    template: behaviors.tmpl\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Consumer selection tests
// ---------------------------------------------------------------------------

// TestWizardRun_ConsumerAutoSelected verifies that when exactly one feature kit
// provides a consumer, it is auto-selected without prompting and injected into
// the scaffold template.
func TestWizardRun_ConsumerAutoSelected(t *testing.T) {
	kitsDir := t.TempDir()
	// Feature kit: provides_consumer, auto-detected via claude-marker.txt
	createFakeKitWithConsumer(t, kitsDir, "github.com/test/repo/claude-kit", "claude-code-kit", "claude-marker.txt", "claude-code")
	// Behavior kit: scaffold template uses {{.Consumer}}
	createFakeScaffoldKitConsumerTemplate(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "claude-marker.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Input: project name (default), keep kit defaults (claude-kit auto-selected), accept behavior kit (Y)
	// Consumer: auto-selected — no additional input line needed
	input := "consumer-project\n\nY\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Output should mention the consumer name
	if !strings.Contains(out.String(), "claude-code") {
		t.Errorf("expected 'claude-code' in output, got:\n%s", out.String())
	}

	// Verify Consumer was injected into the scaffold template
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
	devBehavior, ok := proj.TaskBehaviors["dev"]
	if !ok {
		t.Fatalf("expected 'dev' in task_behaviors, got keys: %v", mapKeys(proj.TaskBehaviors))
	}
	devMap, ok := devBehavior.(map[string]any)
	if !ok {
		t.Fatalf("expected dev to be map, got %T", devBehavior)
	}
	if devMap["consumer"] != "claude-code" {
		t.Errorf("consumer = %v, want 'claude-code'", devMap["consumer"])
	}
}

// TestWizardRun_ConsumerMenu verifies that when two feature kits provide consumers,
// a menu is shown and the user's choice is injected into the scaffold template.
func TestWizardRun_ConsumerMenu(t *testing.T) {
	kitsDir := t.TempDir()
	// Two feature kits with consumers (lexicographic order: kit-a-consumer < kit-b-consumer)
	createFakeKitWithConsumer(t, kitsDir, "github.com/test/repo/kit-a-consumer", "agent-a-kit", "marker-a.txt", "agent-a")
	createFakeKitWithConsumer(t, kitsDir, "github.com/test/repo/kit-b-consumer", "agent-b-kit", "marker-b.txt", "agent-b")
	// Behavior kit
	createFakeScaffoldKitConsumerTemplate(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "marker-a.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "marker-b.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Input: default name, keep kit defaults (both auto-selected), accept behavior kit, select consumer #2 (agent-b)
	input := "\n\nY\n2\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Verify consumer #2 (agent-b) was injected
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
	devBehavior, ok := proj.TaskBehaviors["dev"]
	if !ok {
		t.Fatalf("expected 'dev' in task_behaviors")
	}
	devMap := devBehavior.(map[string]any)
	if devMap["consumer"] != "agent-b" {
		t.Errorf("consumer = %v, want 'agent-b'", devMap["consumer"])
	}
}

// TestWizardRun_ConsumerNone verifies that when no feature kit provides a consumer,
// the Consumer field is empty and {{.Consumer}} renders to an empty string without error.
func TestWizardRun_ConsumerNone(t *testing.T) {
	kitsDir := t.TempDir()
	// Feature kit without provides_consumer
	createFakeKit(t, kitsDir, "github.com/test/repo/go-kit", "go-kit", "go.mod", false)
	// Behavior kit with {{.Consumer}} template
	createFakeScaffoldKitConsumerTemplate(t, kitsDir, "github.com/test/repo/dev-kit", "dev-kit")
	initFakeGitRepo(t, kitsDir, "github.com/test/repo")

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Input: default name, keep kit defaults, accept behavior kit
	// No consumer prompt since no consumer kits selected
	input := "\n\nY\n"
	var out bytes.Buffer

	w := &initwizard.Wizard{In: strings.NewReader(input), Out: &out, KitsDir: kitsDir}
	if err := w.Run(projectDir); err != nil {
		t.Fatalf("Run: %v\nOutput:\n%s", err, out.String())
	}

	// Verify Consumer is empty (renders to empty string, not error)
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
	devBehavior, ok := proj.TaskBehaviors["dev"]
	if !ok {
		t.Fatalf("expected 'dev' in task_behaviors")
	}
	devMap := devBehavior.(map[string]any)
	// consumer key should be nil/empty (YAML renders "" as null or empty string)
	if v := devMap["consumer"]; v != nil && v != "" {
		t.Errorf("consumer = %v, want empty/nil", v)
	}
}

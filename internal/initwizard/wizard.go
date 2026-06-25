package initwizard

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

//go:embed default_behaviors.tmpl
var defaultBehaviorsTmpl string

// ScaffoldTemplateData is the data passed to scaffold templates.
type ScaffoldTemplateData struct {
	ProjectID   string
	ProjectName string
	Agent       string
}

// Wizard runs the project initialization flow. After the kit/workspace/project
// reorg, project.yaml is portable: it holds only id / name / worktree /
// task_behaviors. Kit selection has moved to `boid workspace configure`, so the
// wizard no longer prompts for kits.
type Wizard struct {
	In  io.Reader
	Out io.Writer
}

// projectFileOut is the output structure for project.yaml.
//
// Per docs/plans/kit-workspace-project-reorg.md (削除キー化するフィールド),
// project.yaml must NOT contain `kits`, `env`, `host_commands`,
// `additional_bindings`, `secret_namespace`, or `capabilities`. Those have
// moved to workspace.yaml / kit.yaml.
type projectFileOut struct {
	ID            string         `yaml:"id"`
	Name          string         `yaml:"name"`
	Worktree      bool           `yaml:"worktree"`
	TaskBehaviors map[string]any `yaml:"task_behaviors,omitempty"`
}

// ExpandScaffoldTemplate executes the built-in default_behaviors.tmpl with data
// and parses the result as a map[string]interface{} representing task_behaviors.
func ExpandScaffoldTemplate(data ScaffoldTemplateData) (map[string]any, error) {
	tmpl, err := template.New("scaffold").Parse(defaultBehaviorsTmpl)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}

	var out map[string]any
	if err := yaml.Unmarshal([]byte(sb.String()), &out); err != nil {
		return nil, fmt.Errorf("parse template output as YAML: %w", err)
	}
	return out, nil
}

// Run writes a static project.yaml template into projectDir/.boid/. The only
// interactive prompt is the project name; everything else (executor /
// supervisor behaviors, worktree=true) comes from the embedded template.
func (w *Wizard) Run(projectDir string) error {
	scanner := bufio.NewScanner(w.In)

	// [1] Project name (only prompt — everything else is static template).
	name := w.promptProjectName(scanner, projectDir)

	// [2] Generate project ID and expand built-in scaffold template.
	projectID := uuid.New().String()

	tplData := ScaffoldTemplateData{
		ProjectID:   projectID,
		ProjectName: name,
	}

	taskBehaviors, err := ExpandScaffoldTemplate(tplData)
	if err != nil {
		return fmt.Errorf("expand scaffold template: %w", err)
	}

	// [3] Write project.yaml and create directories.
	boidDir := filepath.Join(projectDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		return fmt.Errorf("create .boid: %w", err)
	}

	proj := projectFileOut{
		ID:            projectID,
		Name:          name,
		Worktree:      true,
		TaskBehaviors: taskBehaviors,
	}

	data, err := yaml.Marshal(proj)
	if err != nil {
		return fmt.Errorf("marshal project.yaml: %w", err)
	}

	projectYAMLPath := filepath.Join(boidDir, "project.yaml")
	if err := os.WriteFile(projectYAMLPath, data, 0o644); err != nil {
		return fmt.Errorf("write project.yaml: %w", err)
	}

	fmt.Fprintf(w.Out, "\n✓ Created %s\n", projectYAMLPath)
	return nil
}

func (w *Wizard) promptProjectName(scanner *bufio.Scanner, projectDir string) string {
	defaultName := filepath.Base(projectDir)
	fmt.Fprintf(w.Out, "Project name [%s]: ", defaultName)
	if scanner.Scan() {
		if input := strings.TrimSpace(scanner.Text()); input != "" {
			return input
		}
	}
	return defaultName
}

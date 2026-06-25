package initwizard

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

//go:embed default_behaviors.tmpl
var defaultBehaviorsTmpl string

// KitInfo holds a discovered kit's reference, directory, and parsed metadata.
type KitInfo struct {
	Ref  string
	Dir  string
	Meta *orchestrator.KitMeta
}

// ScaffoldTemplateData is the data passed to scaffold templates.
type ScaffoldTemplateData struct {
	ProjectID   string
	ProjectName string
	Agent       string
}

// Wizard runs the interactive project initialization flow.
type Wizard struct {
	In      io.Reader
	Out     io.Writer
	KitsDir string
}

// projectFileOut is the output structure for project.yaml.
type projectFileOut struct {
	ID            string         `yaml:"id"`
	Name          string         `yaml:"name"`
	Worktree      bool           `yaml:"worktree"`
	Kits          []string       `yaml:"kits,omitempty"`
	TaskBehaviors map[string]any `yaml:"task_behaviors,omitempty"`
}

// ListAllKits returns all kits found in the registry.
// It looks for kit.yaml directly under each kit directory in BaseDir.
func ListAllKits(reg *orchestrator.KitRegistry) ([]KitInfo, error) {
	names, err := reg.List()
	if err != nil {
		return nil, err
	}

	var kits []KitInfo
	for _, name := range names {
		kitDir := filepath.Join(reg.BaseDir, name)
		meta, readErr := orchestrator.ReadKitMeta(kitDir)
		if readErr != nil {
			continue // skip invalid kits silently
		}
		kits = append(kits, KitInfo{Ref: name, Dir: kitDir, Meta: meta})
	}
	return kits, nil
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

// Run executes the interactive wizard in projectDir, writing .boid/project.yaml.
func (w *Wizard) Run(projectDir string) error {
	scanner := bufio.NewScanner(w.In)

	// [1] Project name
	name := w.promptProjectName(scanner, projectDir)

	// [2] Load installed kits
	reg := orchestrator.NewRegistry(w.KitsDir)
	allKits, err := ListAllKits(reg)
	if err != nil {
		return fmt.Errorf("list kits: %w", err)
	}

	// [3] Select kits (all installed kits are available; no auto-detect)
	selectedKits := w.selectKits(scanner, allKits)

	// [4] Generate project ID and expand built-in scaffold template
	projectID := uuid.New().String()

	tplData := ScaffoldTemplateData{
		ProjectID:   projectID,
		ProjectName: name,
	}

	taskBehaviors, err := ExpandScaffoldTemplate(tplData)
	if err != nil {
		return fmt.Errorf("expand scaffold template: %w", err)
	}

	// [5] Write project.yaml and create directories
	boidDir := filepath.Join(projectDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		return fmt.Errorf("create .boid: %w", err)
	}

	kitRefs := make([]string, 0, len(selectedKits))
	for _, ki := range selectedKits {
		kitRefs = append(kitRefs, ki.Ref)
	}

	proj := projectFileOut{
		ID:            projectID,
		Name:          name,
		Worktree:      true,
		Kits:          kitRefs,
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

func (w *Wizard) selectKits(scanner *bufio.Scanner, kits []KitInfo) []KitInfo {
	if len(kits) == 0 {
		return nil
	}

	selected := make([]bool, len(kits))

	fmt.Fprintln(w.Out, "\nAvailable kits:")
	for i, ki := range kits {
		displayName := kitDisplayName(ki)
		fmt.Fprintf(w.Out, "  [ ] %d. %s (%s)\n", i+1, displayName, ki.Ref)
	}
	fmt.Fprintln(w.Out, "Select kits (space-separated numbers, Enter to skip):")
	fmt.Fprint(w.Out, "> ")

	if !scanner.Scan() {
		return filterSelected(kits, selected)
	}

	for _, tok := range strings.Fields(scanner.Text()) {
		deselect := false
		s := tok
		if strings.HasPrefix(s, "-") {
			deselect = true
			s = s[1:]
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > len(kits) {
			continue
		}
		selected[n-1] = !deselect
	}

	return filterSelected(kits, selected)
}

func kitDisplayName(ki KitInfo) string {
	if ki.Meta.Meta != nil && ki.Meta.Meta.Name != "" {
		return ki.Meta.Meta.Name
	}
	parts := strings.Split(ki.Ref, "/")
	return parts[len(parts)-1]
}

func filterSelected(kits []KitInfo, selected []bool) []KitInfo {
	var result []KitInfo
	for i, ki := range kits {
		if selected[i] {
			result = append(result, ki)
		}
	}
	return result
}

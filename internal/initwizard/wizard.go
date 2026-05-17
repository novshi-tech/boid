package initwizard

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/kit"
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

// ListAllKits returns all kits found in the registry by walking repo directories.
func ListAllKits(reg *orchestrator.KitRegistry) ([]KitInfo, error) {
	repos, err := reg.List()
	if err != nil {
		return nil, err
	}

	var kits []KitInfo
	for _, repoRef := range repos {
		repoDir := filepath.Join(reg.BaseDir, repoRef)
		walkErr := filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			if d.Name() == "kit.yaml" && !d.IsDir() {
				kitDir := filepath.Dir(path)
				rel, relErr := filepath.Rel(reg.BaseDir, kitDir)
				if relErr != nil {
					return nil
				}
				meta, readErr := orchestrator.ReadKitMeta(kitDir)
				if readErr != nil {
					return nil // skip invalid kits silently
				}
				kits = append(kits, KitInfo{Ref: rel, Dir: kitDir, Meta: meta})
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
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

	// [3] Select project-scope kits (all kits are project-scope kits now that
	// the scaffold/behavior-provider step is removed).
	var projectScopeKits []KitInfo
	for _, ki := range allKits {
		if orchestrator.IsProjectScopable(ki.Meta) == nil {
			projectScopeKits = append(projectScopeKits, ki)
		}
	}

	selectedProjectKits := w.selectProjectScopeKits(scanner, projectDir, projectScopeKits)

	// [4] Select agent from selected project-scope kits that provide one
	agent := w.selectAgent(scanner, selectedProjectKits)

	// Build metadata list for validation
	var selectedKitMetas []orchestrator.KitMeta
	for _, ki := range selectedProjectKits {
		selectedKitMetas = append(selectedKitMetas, *ki.Meta)
	}

	// [5] Validate requirements
	fmt.Fprintln(w.Out, "\nChecking requirements...")
	reqErrs := kit.ValidateRequirements(selectedKitMetas)
	if len(reqErrs) > 0 {
		for _, e := range reqErrs {
			fmt.Fprintf(w.Out, "  ✗ %s が PATH 上に見つかりません\n", e.Command)
		}
		return fmt.Errorf("missing required commands; install them and retry")
	}
	for _, km := range selectedKitMetas {
		if km.Requires == nil {
			continue
		}
		for _, cmd := range km.Requires.Commands {
			if path, lookErr := exec.LookPath(cmd); lookErr == nil {
				fmt.Fprintf(w.Out, "  ✓ %s (%s)\n", cmd, path)
			}
		}
	}

	// [6] Generate project ID and expand built-in scaffold template
	projectID := uuid.New().String()

	tplData := ScaffoldTemplateData{
		ProjectID:   projectID,
		ProjectName: name,
		Agent:       agent,
	}

	taskBehaviors, err := ExpandScaffoldTemplate(tplData)
	if err != nil {
		return fmt.Errorf("expand scaffold template: %w", err)
	}

	// [7] Write project.yaml and create directories
	boidDir := filepath.Join(projectDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		return fmt.Errorf("create .boid: %w", err)
	}

	kitRefs := make([]string, 0, len(selectedProjectKits))
	for _, ki := range selectedProjectKits {
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

func (w *Wizard) selectProjectScopeKits(scanner *bufio.Scanner, projectDir string, kits []KitInfo) []KitInfo {
	if len(kits) == 0 {
		return nil
	}

	selected := make([]bool, len(kits))
	results := make([]kit.DetectResult, len(kits))
	for i, ki := range kits {
		results[i] = kit.Detect(projectDir, ki.Dir, *ki.Meta)
		// Required kits are pre-selected; optional kits are shown but not
		// selected by default.
		selected[i] = (results[i] == kit.DetectRequired)
	}

	fmt.Fprintln(w.Out, "\nAvailable kits (auto-detected marked with ✓):")
	for i, ki := range kits {
		mark := " "
		if selected[i] {
			mark = "✓"
		}
		displayName := kitDisplayName(ki)
		suffix := ""
		if results[i] == kit.DetectOptional {
			suffix = " (optional)"
		}
		fmt.Fprintf(w.Out, "  [%s] %d. %s (%s)%s\n", mark, i+1, displayName, ki.Ref, suffix)
	}
	fmt.Fprintln(w.Out, "Enable/disable kits (space-separated numbers, prefix - to deselect, Enter to keep defaults):")
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

func (w *Wizard) selectAgent(scanner *bufio.Scanner, kits []KitInfo) string {
	var agentKits []KitInfo
	for _, ki := range kits {
		if ki.Meta.ProvidesAgent != "" {
			agentKits = append(agentKits, ki)
		}
	}

	switch len(agentKits) {
	case 0:
		return ""
	case 1:
		name := agentKits[0].Meta.ProvidesAgent
		fmt.Fprintf(w.Out, "\nUsing agent: %s\n", name)
		return name
	default:
		fmt.Fprintln(w.Out, "\nSelect default AI agent:")
		for i, ki := range agentKits {
			fmt.Fprintf(w.Out, "  %d. %s\n", i+1, ki.Meta.ProvidesAgent)
		}
		fmt.Fprint(w.Out, "Choice [1]: ")

		if !scanner.Scan() {
			return agentKits[0].Meta.ProvidesAgent
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			return agentKits[0].Meta.ProvidesAgent
		}
		n, err := strconv.Atoi(input)
		if err != nil || n < 1 || n > len(agentKits) {
			return agentKits[0].Meta.ProvidesAgent
		}
		return agentKits[n-1].Meta.ProvidesAgent
	}
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

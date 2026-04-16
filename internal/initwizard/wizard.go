package initwizard

import (
	"bufio"
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
	Consumer    string
	FeatureKits []string
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

// ExpandScaffoldTemplate reads a template file relative to kitDir, executes it
// with data, and parses the result as a map[string]interface{} representing task_behaviors.
func ExpandScaffoldTemplate(kitDir, templatePath string, data ScaffoldTemplateData) (map[string]any, error) {
	fullPath := filepath.Join(kitDir, templatePath)
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("read template %q: %w", fullPath, err)
	}

	tmpl, err := template.New("scaffold").Parse(string(raw))
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

	// Partition kits: scaffold.task_behaviors providers vs feature kits
	var featureKits []KitInfo
	var behaviorKits []KitInfo
	for _, ki := range allKits {
		if ki.Meta.Scaffold != nil && ki.Meta.Scaffold.TaskBehaviors != nil {
			behaviorKits = append(behaviorKits, ki)
		} else {
			featureKits = append(featureKits, ki)
		}
	}

	// [3] Select feature kits
	selectedFeatureKits := w.selectFeatureKits(scanner, projectDir, featureKits)

	// [4] Select behavior kit
	selectedBehaviorKit := w.selectBehaviorKit(scanner, behaviorKits)

	// [4.5] Select consumer from feature kits that provide one
	consumer := w.selectConsumer(scanner, selectedFeatureKits)

	// Build metadata list for validation
	var selectedKitMetas []orchestrator.KitMeta
	for _, ki := range selectedFeatureKits {
		selectedKitMetas = append(selectedKitMetas, *ki.Meta)
	}
	if selectedBehaviorKit != nil {
		selectedKitMetas = append(selectedKitMetas, *selectedBehaviorKit.Meta)
	}

	// [5] Validate requirements
	fmt.Fprintln(w.Out, "\nChecking requirements...")
	reqErrs := kit.ValidateRequirements(selectedKitMetas)
	if len(reqErrs) > 0 {
		for _, e := range reqErrs {
			fmt.Fprintf(w.Out, "  \u2717 %s が PATH 上に見つかりません\n", e.Command)
		}
		return fmt.Errorf("missing required commands; install them and retry")
	}
	for _, km := range selectedKitMetas {
		if km.Requires == nil {
			continue
		}
		for _, cmd := range km.Requires.Commands {
			if path, lookErr := exec.LookPath(cmd); lookErr == nil {
				fmt.Fprintf(w.Out, "  \u2713 %s (%s)\n", cmd, path)
			}
		}
	}

	// [6] Generate project ID and expand scaffold template
	projectID := uuid.New().String()

	featureKitRefs := make([]string, 0, len(selectedFeatureKits))
	for _, ki := range selectedFeatureKits {
		featureKitRefs = append(featureKitRefs, ki.Ref)
	}

	var taskBehaviors map[string]any
	if selectedBehaviorKit != nil &&
		selectedBehaviorKit.Meta.Scaffold != nil &&
		selectedBehaviorKit.Meta.Scaffold.TaskBehaviors != nil {
		tplData := ScaffoldTemplateData{
			ProjectID:   projectID,
			ProjectName: name,
			Consumer:    consumer,
			FeatureKits: featureKitRefs,
		}
		expanded, expandErr := ExpandScaffoldTemplate(
			selectedBehaviorKit.Dir,
			selectedBehaviorKit.Meta.Scaffold.TaskBehaviors.Template,
			tplData,
		)
		if expandErr != nil {
			return fmt.Errorf("expand scaffold template: %w", expandErr)
		}
		taskBehaviors = expanded
	}

	// [7] Write project.yaml and create directories
	boidDir := filepath.Join(projectDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		return fmt.Errorf("create .boid: %w", err)
	}

	proj := projectFileOut{
		ID:            projectID,
		Name:          name,
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

	fmt.Fprintf(w.Out, "\n\u2713 Created %s\n", projectYAMLPath)
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

func (w *Wizard) selectFeatureKits(scanner *bufio.Scanner, projectDir string, kits []KitInfo) []KitInfo {
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

func (w *Wizard) selectBehaviorKit(scanner *bufio.Scanner, kits []KitInfo) *KitInfo {
	if len(kits) == 0 {
		return nil
	}

	if len(kits) == 1 {
		name := kitDisplayName(kits[0])
		desc := ""
		if kits[0].Meta.Scaffold != nil && kits[0].Meta.Scaffold.TaskBehaviors != nil {
			desc = kits[0].Meta.Scaffold.TaskBehaviors.Description
		}
		fmt.Fprintf(w.Out, "\nTask behavior provider: %s", name)
		if desc != "" {
			fmt.Fprintf(w.Out, " - %s", desc)
		}
		fmt.Fprintln(w.Out)
		fmt.Fprint(w.Out, "Use this? [Y/n]: ")
		if scanner.Scan() {
			ans := strings.TrimSpace(scanner.Text())
			if strings.EqualFold(ans, "n") {
				return nil
			}
		}
		return &kits[0]
	}

	// Multiple behavior kits: show menu
	fmt.Fprintln(w.Out, "\nSelect task behavior provider:")
	fmt.Fprintln(w.Out, "  0. None (write task_behaviors manually)")
	for i, ki := range kits {
		name := kitDisplayName(ki)
		desc := ""
		if ki.Meta.Scaffold != nil && ki.Meta.Scaffold.TaskBehaviors != nil {
			desc = ki.Meta.Scaffold.TaskBehaviors.Description
		}
		if desc != "" {
			fmt.Fprintf(w.Out, "  %d. %s - %s\n", i+1, name, desc)
		} else {
			fmt.Fprintf(w.Out, "  %d. %s (%s)\n", i+1, name, ki.Ref)
		}
	}
	fmt.Fprint(w.Out, "Choice [0]: ")

	if !scanner.Scan() {
		return nil
	}
	input := strings.TrimSpace(scanner.Text())
	if input == "" || input == "0" {
		return nil
	}
	n, err := strconv.Atoi(input)
	if err != nil || n < 1 || n > len(kits) {
		return nil
	}
	return &kits[n-1]
}

func (w *Wizard) selectConsumer(scanner *bufio.Scanner, kits []KitInfo) string {
	var consumerKits []KitInfo
	for _, ki := range kits {
		if ki.Meta.ProvidesConsumer != "" {
			consumerKits = append(consumerKits, ki)
		}
	}

	switch len(consumerKits) {
	case 0:
		return ""
	case 1:
		name := consumerKits[0].Meta.ProvidesConsumer
		fmt.Fprintf(w.Out, "\nUsing consumer: %s\n", name)
		return name
	default:
		fmt.Fprintln(w.Out, "\nSelect default AI agent (consumer):")
		for i, ki := range consumerKits {
			fmt.Fprintf(w.Out, "  %d. %s\n", i+1, ki.Meta.ProvidesConsumer)
		}
		fmt.Fprint(w.Out, "Choice [1]: ")

		if !scanner.Scan() {
			return consumerKits[0].Meta.ProvidesConsumer
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			return consumerKits[0].Meta.ProvidesConsumer
		}
		n, err := strconv.Atoi(input)
		if err != nil || n < 1 || n > len(consumerKits) {
			return consumerKits[0].Meta.ProvidesConsumer
		}
		return consumerKits[n-1].Meta.ProvidesConsumer
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

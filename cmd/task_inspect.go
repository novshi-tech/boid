package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// --- command declarations ---

var taskFindingsCmd = &cobra.Command{
	Use:   "findings <id>",
	Short: "Show verification findings for a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskFindings,
}

var taskArtifactsCmd = &cobra.Command{
	Use:   "artifacts <id>",
	Short: "Show artifact payload for a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskArtifacts,
}

var taskTreeCmd = &cobra.Command{
	Use:   "tree [<id>]",
	Short: "Show task hierarchy as a tree",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runTaskTree,
}

func init() {
	taskFindingsCmd.Flags().Bool("all", false, "Show all findings including resolved")
	taskFindingsCmd.Flags().String("status", "", "Filter by status: open or resolved")
	taskArtifactsCmd.Flags().String("field", "", "Extract a specific field from artifact (dot-separated path)")
	taskArtifactsCmd.Flags().String("output-file", "", "Write output to file instead of stdout")
	taskCmd.AddCommand(taskFindingsCmd, taskArtifactsCmd, taskTreeCmd)
}

// --- findings ---

type findingEntry struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

type verificationAgent struct {
	SourceState string         `json:"source_state"`
	Findings    []findingEntry `json:"findings"`
}

func runTaskFindings(cmd *cobra.Command, args []string) error {
	showAll, _ := cmd.Flags().GetBool("all")
	statusFilter, _ := cmd.Flags().GetString("status")

	c := client.NewUnixClient(client.DefaultSocketPath())
	var task orchestrator.Task
	if err := c.Do("GET", "/api/tasks/"+args[0], nil, &task); err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	// parse payload.verification
	var payload map[string]json.RawMessage
	if len(task.Payload) > 0 {
		_ = json.Unmarshal(task.Payload, &payload)
	}

	var verRaw json.RawMessage
	if payload != nil {
		verRaw = payload["verification"]
	}

	type row struct {
		agent       string
		sourceState string
		status      string
		message     string
	}
	var rows []row

	if len(verRaw) > 0 && string(verRaw) != "null" {
		var agents map[string]verificationAgent
		if err := json.Unmarshal(verRaw, &agents); err == nil {
			// sort agent names for stable output
			agentNames := make([]string, 0, len(agents))
			for name := range agents {
				agentNames = append(agentNames, name)
			}
			sort.Strings(agentNames)

			for _, name := range agentNames {
				entry := agents[name]
				for _, f := range entry.Findings {
					// apply filter
					if statusFilter != "" && f.Status != statusFilter {
						continue
					}
					if !showAll && statusFilter == "" && f.Status == "resolved" {
						continue
					}
					rows = append(rows, row{
						agent:       name,
						sourceState: entry.SourceState,
						status:      f.Status,
						message:     f.Message,
					})
				}
			}
		}
	}

	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		fmt.Fprintln(out, "no findings")
		return nil
	}

	fmt.Fprintf(out, "%-24s %-12s %-10s %s\n", "AGENT", "SOURCE_STATE", "STATUS", "MESSAGE")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
	for _, r := range rows {
		fmt.Fprintf(out, "%-24s %-12s %-10s %s\n", r.agent, r.sourceState, r.status, r.message)
	}
	return nil
}

// --- artifacts ---

func runTaskArtifacts(cmd *cobra.Command, args []string) error {
	field, _ := cmd.Flags().GetString("field")
	outputFile, _ := cmd.Flags().GetString("output-file")

	c := client.NewUnixClient(client.DefaultSocketPath())
	var task orchestrator.Task
	if err := c.Do("GET", "/api/tasks/"+args[0], nil, &task); err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	// parse payload.artifact
	var payload map[string]json.RawMessage
	if len(task.Payload) > 0 {
		_ = json.Unmarshal(task.Payload, &payload)
	}

	var artifactRaw json.RawMessage
	if payload != nil {
		artifactRaw = payload["artifact"]
	}

	out := cmd.OutOrStdout()

	if len(artifactRaw) == 0 || string(artifactRaw) == "null" || string(artifactRaw) == "{}" {
		fmt.Fprintln(out, "no artifact")
		return nil
	}

	// --field: extract a specific value
	if field != "" {
		val, err := artifactFieldGet(artifactRaw, field)
		if err != nil {
			return fmt.Errorf("get field %q: %w", field, err)
		}
		if val == nil {
			return fmt.Errorf("field %q not found in artifact", field)
		}

		var text string
		switch v := val.(type) {
		case string:
			text = v
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("marshal field value: %w", err)
			}
			text = string(b)
		}

		if outputFile != "" {
			return os.WriteFile(outputFile, []byte(text), 0o644)
		}
		fmt.Fprint(out, text)
		return nil
	}

	// default: render as YAML
	var v any
	if err := json.Unmarshal(artifactRaw, &v); err != nil {
		return fmt.Errorf("parse artifact: %w", err)
	}
	yamlBytes, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal artifact to YAML: %w", err)
	}
	text := string(yamlBytes)

	if outputFile != "" {
		return os.WriteFile(outputFile, yamlBytes, 0o644)
	}
	fmt.Fprint(out, text)
	return nil
}

// artifactFieldGet extracts a value from an artifact JSON by dot-separated path.
func artifactFieldGet(artifactRaw json.RawMessage, path string) (any, error) {
	var m map[string]any
	if err := json.Unmarshal(artifactRaw, &m); err != nil {
		return nil, err
	}
	segments := strings.Split(path, ".")
	var cur any = m
	for _, seg := range segments {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, nil
		}
		cur = mm[seg]
	}
	return cur, nil
}

// --- tree ---

func runTaskTree(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var tasks []orchestrator.Task
	if err := c.Do("GET", "/api/tasks", nil, &tasks); err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	// build index
	byID := make(map[string]*orchestrator.Task, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}

	// build children map
	children := make(map[string][]*orchestrator.Task)
	for i := range tasks {
		t := &tasks[i]
		if t.ParentID != "" {
			children[t.ParentID] = append(children[t.ParentID], t)
		}
	}

	out := cmd.OutOrStdout()

	if len(args) == 1 {
		// subtree rooted at specified ID
		root, ok := byID[args[0]]
		if !ok {
			return fmt.Errorf("task %q not found", args[0])
		}
		printTree(out, root, children, "", true)
		return nil
	}

	// all tasks: show roots (no parent_id AND no depends_on)
	var roots []*orchestrator.Task
	for i := range tasks {
		t := &tasks[i]
		if t.ParentID == "" && len(t.DependsOn) == 0 {
			roots = append(roots, t)
		}
	}

	if len(roots) == 0 {
		fmt.Fprintln(out, "no tasks")
		return nil
	}

	for i, root := range roots {
		isLast := i == len(roots)-1
		printTree(out, root, children, "", isLast)
	}
	return nil
}

func printTree(out io.Writer, task *orchestrator.Task, children map[string][]*orchestrator.Task, prefix string, isLast bool) {
	connector := "├─"
	childPrefix := prefix + "│  "
	if isLast {
		connector = "└─"
		childPrefix = prefix + "   "
	}

	fmt.Fprintf(out, "%s%s %s %-12s %s\n", prefix, connector, task.ID, task.Status, task.Title)

	kids := children[task.ID]
	for i, child := range kids {
		printTree(out, child, children, childPrefix, i == len(kids)-1)
	}
}

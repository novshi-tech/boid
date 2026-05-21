package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// --- command declarations ---

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
	taskArtifactsCmd.Flags().String("field", "", "Extract a specific field from artifact (dot-separated path)")
	taskArtifactsCmd.Flags().String("output-file", "", "Write output to file instead of stdout")
	taskCmd.AddCommand(taskArtifactsCmd, taskTreeCmd)
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

	if len(artifactRaw) == 0 || string(artifactRaw) == "null" || string(artifactRaw) == "{}" {
		return renderOutput(cmd, nil, func() error {
			fmt.Fprintln(cmd.OutOrStdout(), "no artifact")
			return nil
		})
	}

	// --field: extract a specific value (always plain regardless of --output)
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
		fmt.Fprint(cmd.OutOrStdout(), text)
		return nil
	}

	// parse artifact for output
	var v any
	if err := json.Unmarshal(artifactRaw, &v); err != nil {
		return fmt.Errorf("parse artifact: %w", err)
	}

	// --output-file: always write as YAML (independent of --output format)
	if outputFile != "" {
		yamlBytes, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal artifact to YAML: %w", err)
		}
		return os.WriteFile(outputFile, yamlBytes, 0o644)
	}

	return renderOutput(cmd, v, func() error {
		yamlBytes, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal artifact to YAML: %w", err)
		}
		fmt.Fprint(cmd.OutOrStdout(), string(yamlBytes))
		return nil
	})
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

	type treeNodeOutput struct {
		ID       string            `json:"id"                yaml:"id"`
		Status   string            `json:"status"            yaml:"status"`
		Title    string            `json:"title"             yaml:"title"`
		Children []*treeNodeOutput `json:"children,omitempty" yaml:"children,omitempty"`
	}

	var buildNode func(t *orchestrator.Task) *treeNodeOutput
	buildNode = func(t *orchestrator.Task) *treeNodeOutput {
		node := &treeNodeOutput{
			ID:     t.ID,
			Status: string(t.Status),
			Title:  t.Title,
		}
		for _, child := range children[t.ID] {
			node.Children = append(node.Children, buildNode(child))
		}
		return node
	}

	out := cmd.OutOrStdout()

	if len(args) == 1 {
		root, ok := byID[args[0]]
		if !ok {
			return fmt.Errorf("task %q not found", args[0])
		}
		return renderOutput(cmd, buildNode(root), func() error {
			printTree(out, root, children, "", true)
			return nil
		})
	}

	// all tasks: show roots (no parent_id)
	var roots []*orchestrator.Task
	for i := range tasks {
		t := &tasks[i]
		if t.ParentID == "" {
			roots = append(roots, t)
		}
	}

	rootNodes := make([]*treeNodeOutput, 0, len(roots))
	for _, root := range roots {
		rootNodes = append(rootNodes, buildNode(root))
	}

	return renderOutput(cmd, rootNodes, func() error {
		if len(roots) == 0 {
			fmt.Fprintln(out, "no tasks")
			return nil
		}
		for i, root := range roots {
			isLast := i == len(roots)-1
			printTree(out, root, children, "", isLast)
		}
		return nil
	})
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

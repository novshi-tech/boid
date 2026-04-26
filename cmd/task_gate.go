package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var taskGateCmd = &cobra.Command{
	Use:   "gate",
	Short: "Manage task gates",
}

var taskGateReplayCmd = &cobra.Command{
	Use:   "replay <task-id> <gate-id>",
	Short: "Replay a specific gate for a task",
	Long: `Replay a single gate in isolation. By default the task's current status
is used. Use --status to override (e.g. to recover from aborted/done states).`,
	Args: cobra.ExactArgs(2),
	RunE: runTaskGateReplay,
}

var taskGateListCmd = &cobra.Command{
	Use:   "list <task-id>",
	Short: "List gates that match a task's status",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskGateList,
}

func init() {
	taskGateReplayCmd.Flags().String("status", "", "Override task status for replay (e.g. reworking, verifying)")
	taskGateCmd.AddCommand(taskGateReplayCmd, taskGateListCmd)
	taskGateListCmd.Flags().String("status", "", "Status to query gates for (default: task's current status)")
	taskCmd.AddCommand(taskGateCmd)
}

func runTaskGateReplay(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	gateID := args[1]
	status, _ := cmd.Flags().GetString("status")

	c := client.NewUnixClient(client.DefaultSocketPath())

	body := map[string]any{}
	if status != "" {
		body["status"] = status
	}

	path := fmt.Sprintf("/api/tasks/%s/gates/%s/replay", url.PathEscape(taskID), url.PathEscape(gateID))
	var result api.ReplayGateResult
	if err := c.Do("POST", path, body, &result); err != nil {
		return fmt.Errorf("gate replay: %w", err)
	}

	return renderOutput(cmd, &result, func() error {
		task := result.Task
		if task != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "gate replayed: task %s (%s)\n", task.ID, task.Status)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "gate replayed")
		}
		return nil
	})
}

func runTaskGateList(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	status, _ := cmd.Flags().GetString("status")

	c := client.NewUnixClient(client.DefaultSocketPath())

	path := fmt.Sprintf("/api/tasks/%s/gates", url.PathEscape(taskID))
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}

	var gates []orchestrator.Gate
	if err := c.Do("GET", path, nil, &gates); err != nil {
		return fmt.Errorf("gate list: %w", err)
	}

	return renderOutput(cmd, gates, func() error {
		if len(gates) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no matching gates")
			return nil
		}
		for _, g := range gates {
			phase := string(g.Phase)
			if phase == "" {
				phase = "exit"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-8s %s\n", g.ID, phase, formatOnValues(g.On))
		}
		return nil
	})
}

func formatOnValues(on orchestrator.OnValues) string {
	b, _ := json.Marshal(on)
	return string(b)
}

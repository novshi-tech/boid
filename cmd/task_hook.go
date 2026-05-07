package cmd

import (
	"fmt"
	"net/url"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var taskHookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Manage task hooks",
}

var taskHookReplayCmd = &cobra.Command{
	Use:   "replay <task-id> <hook-id>",
	Short: "Replay a specific hook for a task",
	Long: `Replay a single hook in isolation. By default the task's current status
is used. Use --status to override (e.g. to recover from aborted/done states).`,
	Args: cobra.ExactArgs(2),
	RunE: runTaskHookReplay,
}

var taskHookListCmd = &cobra.Command{
	Use:   "list <task-id>",
	Short: "List hooks that match a task's status",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskHookList,
}

func init() {
	taskHookReplayCmd.Flags().String("status", "", "Override task status for replay (e.g. executing)")
	taskHookCmd.AddCommand(taskHookReplayCmd, taskHookListCmd)
	taskHookListCmd.Flags().String("status", "", "Status to query hooks for (default: task's current status)")
	taskCmd.AddCommand(taskHookCmd)
}

func runTaskHookReplay(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	hookID := args[1]
	status, _ := cmd.Flags().GetString("status")

	c := client.NewUnixClient(client.DefaultSocketPath())

	body := map[string]any{}
	if status != "" {
		body["status"] = status
	}

	path := fmt.Sprintf("/api/tasks/%s/hooks/%s/replay", url.PathEscape(taskID), url.PathEscape(hookID))
	var result api.ReplayHookResult
	if err := c.Do("POST", path, body, &result); err != nil {
		return fmt.Errorf("hook replay: %w", err)
	}

	return renderOutput(cmd, &result, func() error {
		task := result.Task
		if task != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "hook replayed: task %s (%s)\n", task.ID, task.Status)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "hook replayed")
		}
		return nil
	})
}

func runTaskHookList(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	status, _ := cmd.Flags().GetString("status")

	c := client.NewUnixClient(client.DefaultSocketPath())

	path := fmt.Sprintf("/api/tasks/%s/hooks", url.PathEscape(taskID))
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}

	var hooks []orchestrator.Hook
	if err := c.Do("GET", path, nil, &hooks); err != nil {
		return fmt.Errorf("hook list: %w", err)
	}

	return renderOutput(cmd, hooks, func() error {
		if len(hooks) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no matching hooks")
			return nil
		}
		for _, h := range hooks {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", h.ID)
		}
		return nil
	})
}

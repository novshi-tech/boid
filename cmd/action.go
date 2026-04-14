package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var actionCmd = &cobra.Command{
	Use:   "action",
	Short: "Manage actions",
}

var actionSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send an action to a task",
	RunE:  runActionSend,
}

func init() {
	actionSendCmd.Flags().String("task", "", "Task ID (required)")
	actionSendCmd.Flags().String("type", "", "Action type (required)")
	actionSendCmd.Flags().String("payload", "", "Payload file (optional, - for stdin)")
	actionCmd.AddCommand(actionSendCmd)
	rootCmd.AddCommand(actionCmd)
}

func runActionSend(cmd *cobra.Command, args []string) error {
	taskID, _ := cmd.Flags().GetString("task")
	actionType, _ := cmd.Flags().GetString("type")
	payloadFile, _ := cmd.Flags().GetString("payload")

	if taskID == "" || actionType == "" {
		return fmt.Errorf("--task and --type are required")
	}

	req := map[string]any{"type": actionType}

	if payloadFile != "" {
		var data []byte
		var err error
		if payloadFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(payloadFile)
		}
		if err != nil {
			return fmt.Errorf("read payload: %w", err)
		}
		var payload json.RawMessage
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("invalid payload JSON: %w", err)
		}
		req["payload"] = payload
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	var result map[string]any
	if err := c.Do("POST", fmt.Sprintf("/api/tasks/%s/actions", taskID), req, &result); err != nil {
		return fmt.Errorf("send action: %w", err)
	}

	if errMsg, ok := result["error"]; ok {
		return fmt.Errorf("%s", errMsg)
	}

	return renderOutput(cmd, result, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "action applied: %s\n", actionType)
		if task, ok := result["task"].(map[string]any); ok {
			fmt.Fprintf(cmd.OutOrStdout(), "task status: %s\n", task["status"])
		}
		if hooks, ok := result["matched_hooks"]; ok {
			fmt.Fprintf(cmd.OutOrStdout(), "matched hooks: %v\n", hooks)
		}
		return nil
	})
}

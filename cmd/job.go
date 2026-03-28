package cmd

import (
	"fmt"
	"strconv"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage jobs",
}

var jobDoneCmd = &cobra.Command{
	Use:   "done <job-id>",
	Short: "Mark a job as complete",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobDone,
}

func init() {
	jobDoneCmd.Flags().Int("exit-code", 0, "Exit code of the job")
	jobCmd.AddCommand(jobDoneCmd)
	rootCmd.AddCommand(jobCmd)
}

func runJobDone(cmd *cobra.Command, args []string) error {
	exitCode, _ := cmd.Flags().GetInt("exit-code")
	jobID := args[0]

	c := client.NewUnixClient(client.DefaultSocketPath())
	req := map[string]any{
		"exit_code": exitCode,
	}

	var result map[string]any
	if err := c.Do("POST", fmt.Sprintf("/api/jobs/%s/done", jobID), req, &result); err != nil {
		return fmt.Errorf("job done: %w", err)
	}

	fmt.Printf("job %s completed (exit_code=%s)\n", jobID, strconv.Itoa(exitCode))
	return nil
}

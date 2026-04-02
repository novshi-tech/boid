package cmd

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/novshi-tech/boid/internal/api"
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

var jobListCmd = &cobra.Command{
	Use:   "list --task <task-id>",
	Short: "List jobs for a task",
	RunE:  runJobList,
}

var jobShowCmd = &cobra.Command{
	Use:   "show <job-id>",
	Short: "Show job details",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobShow,
}

var jobWatchCmd = &cobra.Command{
	Use:   "watch <job-id>",
	Short: "Watch a job until it finishes",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobWatch,
}

func init() {
	jobListCmd.Flags().String("task", "", "Task ID (required)")
	jobDoneCmd.Flags().Int("exit-code", 0, "Exit code of the job")
	jobDoneCmd.Flags().String("output-file", "", "File containing stdout capture to send as output")
	jobWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	jobCmd.AddCommand(jobListCmd, jobShowCmd, jobWatchCmd, jobDoneCmd)
	rootCmd.AddCommand(jobCmd)
}

func runJobList(cmd *cobra.Command, args []string) error {
	taskID, _ := cmd.Flags().GetString("task")
	if taskID == "" {
		return fmt.Errorf("--task is required")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	var jobs []*api.Job
	if err := c.Do("GET", "/api/jobs?task_id="+taskID, nil, &jobs); err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return nil
	}

	renderJobList(jobs)
	return nil
}

func runJobShow(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var job api.Job
	if err := c.Do("GET", "/api/jobs/"+args[0], nil, &job); err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	renderJob(&job)
	return nil
}

func runJobWatch(cmd *cobra.Command, args []string) error {
	interval, _ := cmd.Flags().GetDuration("interval")
	if interval <= 0 {
		return fmt.Errorf("--interval must be positive")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	jobID := args[0]
	var lastFingerprint string

	for {
		var job api.Job
		if err := c.Do("GET", "/api/jobs/"+jobID, nil, &job); err != nil {
			return fmt.Errorf("watch job: %w", err)
		}

		fingerprint := fmt.Sprintf("%s|%d|%s|%s", job.Status, job.ExitCode, formatTime(job.UpdatedAt), job.Output)
		if fingerprint != lastFingerprint {
			printWatchHeader("job", job.ID)
			renderJob(&job)
			fmt.Println()
			lastFingerprint = fingerprint
		}

		if isTerminalJobStatus(job.Status) {
			return nil
		}
		time.Sleep(interval)
	}
}

func runJobDone(cmd *cobra.Command, args []string) error {
	exitCode, _ := cmd.Flags().GetInt("exit-code")
	jobID := args[0]

	req := map[string]any{
		"exit_code": exitCode,
	}

	// Read output file if specified
	outputFile, _ := cmd.Flags().GetString("output-file")
	if outputFile != "" {
		data, err := os.ReadFile(outputFile)
		if err == nil {
			req["output"] = string(data)
		}
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	var result map[string]any
	if err := c.Do("POST", fmt.Sprintf("/api/jobs/%s/done", jobID), req, &result); err != nil {
		return fmt.Errorf("job done: %w", err)
	}

	fmt.Printf("job %s completed (exit_code=%s)\n", jobID, strconv.Itoa(exitCode))
	return nil
}

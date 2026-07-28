package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	Use:         "done <job-id>",
	Short:       "Mark a job as complete",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runJobDone,
}

var jobListCmd = &cobra.Command{
	Use:         "list --task <task-id>",
	Short:       "List jobs for a task",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runJobList,
}

var jobShowCmd = &cobra.Command{
	Use:         "show <job-id>",
	Short:       "Show job details",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runJobShow,
}

var jobWatchCmd = &cobra.Command{
	Use:         "watch <job-id>",
	Short:       "Watch a job until it finishes",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runJobWatch,
}

var jobLogCmd = &cobra.Command{
	Use:         "log <job-id>",
	Short:       "Show transcript log for a job",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runJobLog,
}

func init() {
	jobListCmd.Flags().String("task", "", "Task ID (required)")
	jobDoneCmd.Flags().Int("exit-code", 0, "Exit code of the job")
	jobDoneCmd.Flags().String("output-file", "", "File containing stdout capture to send as output")
	jobWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	jobCmd.AddCommand(jobListCmd, jobShowCmd, jobWatchCmd, jobDoneCmd, jobLogCmd)
	rootCmd.AddCommand(jobCmd)
}

func runJobList(cmd *cobra.Command, args []string) error {
	taskID, _ := cmd.Flags().GetString("task")
	if taskID == "" {
		return fmt.Errorf("--task is required")
	}

	c := client.FromContext(cmd.Context())
	var jobs []*api.Job
	if err := c.Do("GET", "/api/jobs?task_id="+taskID, nil, &jobs); err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	return renderOutput(cmd, jobs, func() error {
		if len(jobs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no jobs")
			return nil
		}
		renderJobList(jobs)
		return nil
	})
}

func runJobShow(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	var job api.Job
	if err := c.Do("GET", "/api/jobs/"+args[0], nil, &job); err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	return renderOutput(cmd, &job, func() error {
		renderJob(&job)
		return nil
	})
}

func runJobWatch(cmd *cobra.Command, args []string) error {
	interval, _ := cmd.Flags().GetDuration("interval")
	if interval <= 0 {
		return fmt.Errorf("--interval must be positive")
	}

	c := client.FromContext(cmd.Context())
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
			if err := renderOutput(cmd, &job, func() error {
				renderJob(&job)
				return nil
			}); err != nil {
				return err
			}
			fmt.Println()
			lastFingerprint = fingerprint
		}

		if isTerminalJobStatus(job.Status) {
			return nil
		}
		time.Sleep(interval)
	}
}

// runJobLog prints a job's transcript, or — when there is none — the
// daemon's own explanation of why.
//
// The 404 body is relayed verbatim rather than replaced with a fixed
// string. GET /jobs/{id}/log answers 404 for three genuinely different
// situations (no such job / the job has no runtime / the transcript is gone
// — see internal/api/job.go's writeJobLogUnavailable), and this command used
// to print "log not available (runtime cleaned up)" for all of them,
// discarding the body. Two of the three were therefore reported as an event
// that never happened, which cost a 2026-07-28 dogfood session real time
// while diagnosing a hung job. A daemon predating that change sends no body
// at all, in which case this prints nothing extra rather than inventing a
// reason it cannot know.
//
// Exit status is deliberately unchanged: every 404 still succeeds. Scripts
// that treat "no log yet" as a non-error (polling a job that has not started
// writing) keep working, and the operator-facing difference now lives where
// it is actually read — in the message.
func runJobLog(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	statusCode, body, err := c.GetRaw("/api/jobs/" + args[0] + "/log")
	if err != nil {
		return fmt.Errorf("get job log: %w", err)
	}
	out := cmd.OutOrStdout()
	if statusCode == 404 {
		if msg := strings.TrimSpace(string(body)); msg != "" {
			fmt.Fprintln(out, msg)
		}
		return nil
	}
	if statusCode >= 400 {
		return fmt.Errorf("server error: HTTP %d: %s", statusCode, string(body))
	}
	out.Write(body) //nolint:errcheck
	return nil
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

	c := client.FromContext(cmd.Context())
	var result map[string]any
	if err := c.Do("POST", fmt.Sprintf("/api/jobs/%s/done", jobID), req, &result); err != nil {
		return fmt.Errorf("job done: %w", err)
	}

	fmt.Printf("job %s completed (exit_code=%s)\n", jobID, strconv.Itoa(exitCode))
	return nil
}

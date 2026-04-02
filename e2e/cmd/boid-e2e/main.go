package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError("command required")
	}

	switch args[0] {
	case "wait-unix-socket":
		return runWaitUnixSocket(args[1:])
	case "wait-health":
		return runWaitHealth(args[1:])
	case "get-task":
		return runGetTask(args[1:])
	case "wait-task-status":
		return runWaitTaskStatus(args[1:])
	case "list-jobs":
		return runListJobs(args[1:])
	case "wait-job-count":
		return runWaitJobCount(args[1:])
	case "assert-job-role-count":
		return runAssertJobRoleCount(args[1:])
	default:
		return usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
}

func runWaitUnixSocket(args []string) error {
	fs := flag.NewFlagSet("wait-unix-socket", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")

	if err := fs.Parse(args); err != nil {
		return err
	}

	socketPath := resolveSocketPath(fs.Args())
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := waitUnixSocket(ctx, socketPath, *interval); err != nil {
		return fmt.Errorf("wait unix socket %s: %w", socketPath, err)
	}
	return nil
}

func runWaitHealth(args []string) error {
	fs := flag.NewFlagSet("wait-health", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")

	if err := fs.Parse(args); err != nil {
		return err
	}

	socketPath := resolveSocketPath(fs.Args())
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := waitHealth(ctx, socketPath, *interval); err != nil {
		return fmt.Errorf("wait health via %s: %w", socketPath, err)
	}
	return nil
}

func runGetTask(args []string) error {
	fs := flag.NewFlagSet("get-task", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("get-task requires <task-id>")
	}

	task, err := getTask(*socketPath, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(task)
}

func runWaitTaskStatus(args []string) error {
	fs := flag.NewFlagSet("wait-task-status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usageError("wait-task-status requires <task-id> <status>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	task, err := waitTaskStatus(ctx, *socketPath, fs.Arg(0), fs.Arg(1), *interval)
	if err != nil {
		return err
	}
	return printJSON(task)
}

func runListJobs(args []string) error {
	fs := flag.NewFlagSet("list-jobs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("list-jobs requires <task-id>")
	}

	jobs, err := listJobs(*socketPath, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(jobs)
}

func runWaitJobCount(args []string) error {
	fs := flag.NewFlagSet("wait-job-count", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usageError("wait-job-count requires <task-id> <count>")
	}

	wantCount, err := strconv.Atoi(fs.Arg(1))
	if err != nil || wantCount < 0 {
		return usageError("wait-job-count requires <task-id> <count>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	jobs, err := waitJobCount(ctx, *socketPath, fs.Arg(0), wantCount, *interval)
	if err != nil {
		return err
	}
	return printJSON(jobs)
}

func runAssertJobRoleCount(args []string) error {
	fs := flag.NewFlagSet("assert-job-role-count", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 3 {
		return usageError("assert-job-role-count requires <task-id> <role> <count>")
	}

	wantCount, err := strconv.Atoi(fs.Arg(2))
	if err != nil || wantCount < 0 {
		return usageError("assert-job-role-count requires <task-id> <role> <count>")
	}

	jobs, err := listJobs(*socketPath, fs.Arg(0))
	if err != nil {
		return err
	}
	gotCount := countJobsByRole(jobs, fs.Arg(1))
	if gotCount != wantCount {
		return fmt.Errorf("job role count mismatch for %q: got %d, want %d", fs.Arg(1), gotCount, wantCount)
	}
	return printJSON(jobs)
}

func resolveSocketPath(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return client.DefaultSocketPath()
}

func waitUnixSocket(ctx context.Context, socketPath string, interval time.Duration) error {
	for {
		conn, err := net.DialTimeout("unix", socketPath, interval)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func waitHealth(ctx context.Context, socketPath string, interval time.Duration) error {
	c := client.NewUnixClient(socketPath)

	for {
		var resp struct {
			Status string `json:"status"`
		}
		err := c.Do("GET", "/api/health", nil, &resp)
		if err == nil && resp.Status == "ok" {
			return nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("%w: last error: %v", ctx.Err(), err)
			}
			return fmt.Errorf("%w: unexpected health status %q", ctx.Err(), resp.Status)
		case <-time.After(interval):
		}
	}
}

func getTask(socketPath, taskID string) (*orchestrator.Task, error) {
	c := client.NewUnixClient(socketPath)
	var task orchestrator.Task
	if err := c.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}
	return &task, nil
}

func waitTaskStatus(ctx context.Context, socketPath, taskID, wantStatus string, interval time.Duration) (*orchestrator.Task, error) {
	for {
		task, err := getTask(socketPath, taskID)
		if err == nil && string(task.Status) == wantStatus {
			return task, nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("%w: last error: %v", ctx.Err(), err)
			}
			return nil, fmt.Errorf("%w: task %s did not reach status %s", ctx.Err(), taskID, wantStatus)
		case <-time.After(interval):
		}
	}
}

func listJobs(socketPath, taskID string) ([]map[string]any, error) {
	c := client.NewUnixClient(socketPath)
	var jobs []map[string]any
	if err := c.Do("GET", "/api/jobs?task_id="+taskID, nil, &jobs); err != nil {
		return nil, fmt.Errorf("list jobs for task %s: %w", taskID, err)
	}
	return jobs, nil
}

func waitJobCount(ctx context.Context, socketPath, taskID string, wantCount int, interval time.Duration) ([]map[string]any, error) {
	for {
		jobs, err := listJobs(socketPath, taskID)
		if err == nil && len(jobs) >= wantCount {
			return jobs, nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("%w: last error: %v", ctx.Err(), err)
			}
			return nil, fmt.Errorf("%w: task %s did not reach job count %d", ctx.Err(), taskID, wantCount)
		case <-time.After(interval):
		}
	}
}

func countJobsByRole(jobs []map[string]any, role string) int {
	count := 0
	for _, job := range jobs {
		if fmt.Sprint(job["role"]) == role {
			count++
		}
	}
	return count
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(v)
}

func usageError(msg string) error {
	return errors.New(msg + "\nusage: boid-e2e <wait-unix-socket|wait-health|get-task|wait-task-status|list-jobs|wait-job-count|assert-job-role-count> ...")
}

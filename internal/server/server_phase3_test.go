package server

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestServerJobRuntimeAttachAndResize(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")
	localRuntime := &dispatcher.LocalRuntime{RootDir: filepath.Join(tmpDir, "runtimes")}

	srv, err := New(Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		JobRuntime: localRuntime,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()

	if err := orchestrator.CreateProject(srv.DB(), &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: tmpDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := orchestrator.CreateTask(srv.DB(), &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Title:     "attach",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	handle, err := localRuntime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "printf 'attach ready'; sleep 0.05; printf ' done'",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	job := &dispatcher.Job{
		ID:          "job-1",
		TaskID:      "task-1",
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        "hook",
		RuntimeID:   handle.ID,
		Interactive: true,
		TTY:         true,
	}
	if err := dispatcher.CreateJob(srv.DB(), job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	c := client.NewUnixClient(sockPath)
	if err := c.ResizeJob(job.ID, 50, 120); err != nil {
		t.Fatalf("ResizeJob: %v", err)
	}

	var out bytes.Buffer
	if err := c.AttachJob(job.ID, nil, &out); err != nil {
		t.Fatalf("AttachJob: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "attach ready") || !strings.Contains(got, "done") {
		t.Fatalf("attach output = %q, want transcript", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := localRuntime.Wait(ctx, handle.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
}

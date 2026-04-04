package sandbox_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestRunBoidShim_JobDoneSendsTypedRequest(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0})
	}()

	outputPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(outputPath, []byte("job output"), 0o644); err != nil {
		t.Fatalf("write output file: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-123")

	resp, err := sandbox.RunBoidShim([]string{"job", "done", "job-1", "--exit-code", "7", "--output-file", outputPath})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}

	req := <-reqCh
	if req.Command != "boid" {
		t.Fatalf("command = %q, want boid", req.Command)
	}
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpJobDone {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpJobDone)
	}
	if req.Boid.JobID != "job-1" {
		t.Fatalf("job id = %q, want job-1", req.Boid.JobID)
	}
	if req.Boid.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", req.Boid.ExitCode)
	}
	if req.Boid.Output != "job output" {
		t.Fatalf("output = %q, want job output", req.Boid.Output)
	}
}

func TestRunBoidShim_TaskCreateSendsTypedRequest(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0})
	}()

	specPath := filepath.Join(dir, "task.yaml")
	specYAML := "project_id: proj-1\ntitle: hello\nbehavior: dev\ndescription: desc\npayload:\n  name: alice\n"
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write task spec: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-456")

	resp, err := sandbox.RunBoidShim([]string{
		"task", "create",
		"-f", specPath,
	})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskCreate {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskCreate)
	}
	if req.Boid.ProjectID != "proj-1" {
		t.Fatalf("project id = %q, want proj-1", req.Boid.ProjectID)
	}
	if req.Boid.Title != "hello" {
		t.Fatalf("title = %q, want hello", req.Boid.Title)
	}
	if req.Boid.Behavior != "dev" {
		t.Fatalf("behavior = %q, want dev", req.Boid.Behavior)
	}
	if req.Boid.Description != "desc" {
		t.Fatalf("description = %q, want desc", req.Boid.Description)
	}
	if string(req.Boid.Payload) != `{"name":"alice"}` {
		t.Fatalf("payload = %s, want %s", string(req.Boid.Payload), `{"name":"alice"}`)
	}
}

func TestRunBoidShim_RejectsUnknownSubcommand(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	if _, err := sandbox.RunBoidShim([]string{"task", "list"}); err == nil {
		t.Fatal("expected error for unsupported subcommand")
	}
}

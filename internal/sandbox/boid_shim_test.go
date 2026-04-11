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

func TestRunBoidShim_TaskCreatePropagatesDependencyFields(t *testing.T) {
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
	specYAML := "project_id: proj-1\n" +
		"title: dependent task\n" +
		"behavior: dev\n" +
		"description: desc\n" +
		"ref: task-c\n" +
		"parent_id: parent-xyz\n" +
		"depends_on:\n  - task-a\n  - task-b\n" +
		"depends_on_payload: artifact.pr.merged\n" +
		"auto_start: true\n"
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write task spec: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-789")

	resp, err := sandbox.RunBoidShim([]string{"task", "create", "-f", specPath})
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
	if req.Boid.Ref != "task-c" {
		t.Errorf("ref = %q, want task-c", req.Boid.Ref)
	}
	if req.Boid.ParentID != "parent-xyz" {
		t.Errorf("parent_id = %q, want parent-xyz", req.Boid.ParentID)
	}
	if got, want := req.Boid.DependsOn, []string{"task-a", "task-b"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("depends_on = %v, want %v", got, want)
	}
	if req.Boid.DependsOnPayload != "artifact.pr.merged" {
		t.Errorf("depends_on_payload = %q, want artifact.pr.merged", req.Boid.DependsOnPayload)
	}
	if !req.Boid.AutoStart {
		t.Errorf("auto_start = false, want true")
	}
}

func TestRunBoidShim_RejectsUnknownSubcommand(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	if _, err := sandbox.RunBoidShim([]string{"task", "list"}); err == nil {
		t.Fatal("expected error for unsupported subcommand")
	}
}

func TestRunBoidShim_TaskUpdateSendsTypedRequest(t *testing.T) {
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

	payloadPath := filepath.Join(dir, "payload.yaml")
	payloadYAML := "artifact:\n  pr:\n    number: 42\n    merged: true\n    url: https://example/42\n"
	if err := os.WriteFile(payloadPath, []byte(payloadYAML), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-upd")

	resp, err := sandbox.RunBoidShim([]string{
		"task", "update", "task-target",
		"--payload-file", payloadPath,
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
	if req.Boid.Op != sandbox.BoidOpTaskUpdate {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskUpdate)
	}
	if req.Boid.TaskID != "task-target" {
		t.Fatalf("task id = %q, want task-target", req.Boid.TaskID)
	}
	if got, want := string(req.Boid.Payload), `{"artifact":{"pr":{"merged":true,"number":42,"url":"https://example/42"}}}`; got != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestRunBoidShim_TaskUpdateRequiresAtLeastOneField(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "task-xyz"}); err == nil {
		t.Fatal("expected error when no --title/--description/--payload-file is given")
	}
}

func TestRunBoidShim_TaskUpdateRequiresTaskID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "--title", "x"}); err == nil {
		t.Fatal("expected error when task id is missing")
	}
}

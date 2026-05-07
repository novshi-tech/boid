package sandbox_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// shim sends os.Executable() (an absolute path); the broker dispatches by
	// req.Boid != nil, so we only verify the request is well-formed.
	if !filepath.IsAbs(req.Command) {
		t.Fatalf("command = %q, want absolute path (os.Executable result)", req.Command)
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
		"depends_on_payload: artifact.auto-merge.merged\n" +
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
	if req.Boid.DependsOnPayload != "artifact.auto-merge.merged" {
		t.Errorf("depends_on_payload = %q, want artifact.auto-merge.merged", req.Boid.DependsOnPayload)
	}
	if !req.Boid.AutoStart {
		t.Errorf("auto_start = false, want true")
	}
}

func TestRunBoidShim_TaskCreatePropagatesBaseBranch(t *testing.T) {
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
		"title: branch override\n" +
		"behavior: dev\n" +
		"base_branch: feature/my-branch\n"
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write task spec: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-base")

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
	if req.Boid.BaseBranch != "feature/my-branch" {
		t.Errorf("base_branch = %q, want feature/my-branch", req.Boid.BaseBranch)
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

func TestRunBoidShim_TaskCreate_BehaviorSpec(t *testing.T) {
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
	specYAML := `project_id: proj-1
title: kit task
behavior_spec:
  name: kit/conflict-fix
  traits:
    - instructions
  worktree: true
`
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write task spec: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-spec")

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
	if req.Boid.Op != sandbox.BoidOpTaskCreate {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskCreate)
	}
	if req.Boid.Behavior != "" {
		t.Errorf("behavior = %q, want empty", req.Boid.Behavior)
	}
	if req.Boid.BehaviorSpec == nil {
		t.Fatal("behavior_spec is nil, want non-nil")
	}
	if req.Boid.BehaviorSpec.Name != "kit/conflict-fix" {
		t.Errorf("behavior_spec.name = %q, want %q", req.Boid.BehaviorSpec.Name, "kit/conflict-fix")
	}
	if !req.Boid.BehaviorSpec.Worktree {
		t.Error("behavior_spec.worktree = false, want true")
	}
}

func TestRunBoidShim_TaskCreate_NeitherBehaviorNorSpec_ForwardsToBroker(t *testing.T) {
	// shim は behavior 省略時に弾かない (サーバ側で DefaultBehavior に routing
	// される)。 broker への forward 経路を fake broker で検証する。
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
	specYAML := "project_id: proj-1\ntitle: triage me\n"
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write task spec: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-noop")

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
	if req.Boid.Op != sandbox.BoidOpTaskCreate {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskCreate)
	}
	if req.Boid.Behavior != "" {
		t.Errorf("behavior = %q, want empty (server side defaults to plan)", req.Boid.Behavior)
	}
	if req.Boid.BehaviorSpec != nil {
		t.Errorf("behavior_spec = %+v, want nil", req.Boid.BehaviorSpec)
	}
	if req.Boid.Title != "triage me" {
		t.Errorf("title = %q, want triage me", req.Boid.Title)
	}
}

func TestRunBoidShim_TaskCreate_BothBehaviorAndSpec(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	dir := t.TempDir()
	specPath := filepath.Join(dir, "task.yaml")
	specYAML := "project_id: proj-1\ntitle: bad task\nbehavior: dev\nbehavior_spec:\n  name: kit/x\n"
	if err := os.WriteFile(specPath, []byte(specYAML), 0o644); err != nil {
		t.Fatalf("write task spec: %v", err)
	}

	if _, err := sandbox.RunBoidShim([]string{"task", "create", "-f", specPath}); err == nil {
		t.Fatal("expected error when both behavior and behavior_spec are set")
	}
}

// --- task import tests ---

func newFakeBrokerForImport(t *testing.T) (sockPath string, reqCh chan sandbox.ExecRequest) {
	t.Helper()
	dir := t.TempDir()
	sockPath = filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})
	reqCh = make(chan sandbox.ExecRequest, 1)
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
	return sockPath, reqCh
}

func redirectStdinForTest(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	if _, err := w.WriteString(content); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	w.Close()
}

func TestParseBoidTaskImport_Stdin(t *testing.T) {
	sockPath, reqCh := newFakeBrokerForImport(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-import")

	ndjson := `{"title":"task1","behavior":"dev"}` + "\n" + `{"title":"task2","behavior":"dev"}` + "\n"
	redirectStdinForTest(t, ndjson)

	resp, err := sandbox.RunBoidShim([]string{"task", "import"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskImport {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskImport)
	}
	if len(req.Boid.ImportTasks) != 2 {
		t.Fatalf("ImportTasks len = %d, want 2", len(req.Boid.ImportTasks))
	}
	if string(req.Boid.ImportTasks[0]) != `{"title":"task1","behavior":"dev"}` {
		t.Errorf("ImportTasks[0] = %s, want %s", string(req.Boid.ImportTasks[0]), `{"title":"task1","behavior":"dev"}`)
	}
	if string(req.Boid.ImportTasks[1]) != `{"title":"task2","behavior":"dev"}` {
		t.Errorf("ImportTasks[1] = %s, want %s", string(req.Boid.ImportTasks[1]), `{"title":"task2","behavior":"dev"}`)
	}
}

func TestParseBoidTaskImport_File(t *testing.T) {
	sockPath, reqCh := newFakeBrokerForImport(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-import-file")

	dir := t.TempDir()
	filePath := filepath.Join(dir, "tasks.jsonl")
	content := `{"title":"fileTask","behavior":"dev","remote_id":"r1"}` + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	resp, err := sandbox.RunBoidShim([]string{"task", "import", "-f", filePath})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskImport {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskImport)
	}
	if len(req.Boid.ImportTasks) != 1 {
		t.Fatalf("ImportTasks len = %d, want 1", len(req.Boid.ImportTasks))
	}
}

func TestParseBoidTaskImport_ProjectOverride(t *testing.T) {
	sockPath, reqCh := newFakeBrokerForImport(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-import-proj")

	redirectStdinForTest(t, `{"title":"t1"}`+"\n")

	resp, err := sandbox.RunBoidShim([]string{"task", "import", "--project=p1"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.ImportProjectOverride != "p1" {
		t.Errorf("ImportProjectOverride = %q, want p1", req.Boid.ImportProjectOverride)
	}
}

func TestParseBoidTaskImport_DatasourceOverride(t *testing.T) {
	sockPath, reqCh := newFakeBrokerForImport(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-import-ds")

	redirectStdinForTest(t, `{"title":"t1"}`+"\n")

	// スペース区切り
	resp, err := sandbox.RunBoidShim([]string{"task", "import", "--datasource", "gh-am"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.ImportDatasourceOverride != "gh-am" {
		t.Errorf("ImportDatasourceOverride = %q, want gh-am", req.Boid.ImportDatasourceOverride)
	}
}

func TestParseBoidTaskImport_EmptyLines(t *testing.T) {
	sockPath, reqCh := newFakeBrokerForImport(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-import-empty")

	// 空行を含む
	ndjson := "\n" + `{"title":"t1"}` + "\n\n" + `{"title":"t2"}` + "\n\n"
	redirectStdinForTest(t, ndjson)

	resp, err := sandbox.RunBoidShim([]string{"task", "import"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if len(req.Boid.ImportTasks) != 2 {
		t.Fatalf("ImportTasks len = %d, want 2 (empty lines skipped)", len(req.Boid.ImportTasks))
	}
}

func TestParseBoidTaskImport_InvalidJSON(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	// 1行目は valid、2行目は invalid
	redirectStdinForTest(t, `{"valid":"json"}`+"\n"+`{not valid json}`+"\n")

	_, err := sandbox.RunBoidShim([]string{"task", "import"})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "line 2: invalid JSON") {
		t.Errorf("error = %q, want 'line 2: invalid JSON'", err.Error())
	}
}

func TestRunBoidShim_TaskReopen_SendsTypedRequest(t *testing.T) {
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

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-reopen")

	resp, err := sandbox.RunBoidShim([]string{"task", "reopen", "task-abc"})
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
	if req.Boid.Op != sandbox.BoidOpTaskReopen {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskReopen)
	}
	if req.Boid.TaskID != "task-abc" {
		t.Fatalf("task id = %q, want task-abc", req.Boid.TaskID)
	}
}

func TestRunBoidShim_TaskReopen_RequiresTaskID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	if _, err := sandbox.RunBoidShim([]string{"task", "reopen"}); err == nil {
		t.Fatal("expected error when task id is missing")
	}
}

func TestParseBoidTaskImport_EmptyBatch(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")

	// 空行のみ
	redirectStdinForTest(t, "\n\n")

	_, err := sandbox.RunBoidShim([]string{"task", "import"})
	if err == nil {
		t.Fatal("expected error for empty batch")
	}
	if !strings.Contains(err.Error(), "at least one task") {
		t.Errorf("error = %q, want 'at least one task'", err.Error())
	}
}

func newFakeBrokerSingle(t *testing.T) (sockPath string, reqCh chan sandbox.ExecRequest) {
	t.Helper()
	dir := t.TempDir()
	sockPath = filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close(); os.Remove(sockPath) })
	reqCh = make(chan sandbox.ExecRequest, 1)
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
	return sockPath, reqCh
}

func TestRunBoidShim_TaskNotify_AskMode(t *testing.T) {
	sockPath, reqCh := newFakeBrokerSingle(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-notify")

	resp, err := sandbox.RunBoidShim([]string{
		"task", "notify", "task-xyz",
		"--message", "Plan ready",
		"--ask", "Approve?",
		"--question-id", "q-001",
	})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskNotify {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskNotify)
	}
	if req.Boid.Message != "Plan ready" {
		t.Errorf("message = %q, want Plan ready", req.Boid.Message)
	}
	if req.Boid.Ask != "Approve?" {
		t.Errorf("ask = %q, want Approve?", req.Boid.Ask)
	}
	if req.Boid.QuestionID != "q-001" {
		t.Errorf("question_id = %q, want q-001", req.Boid.QuestionID)
	}
}

func TestRunBoidShim_TaskNotify_NormalMode_NoAsk(t *testing.T) {
	sockPath, reqCh := newFakeBrokerSingle(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-notify2")

	resp, err := sandbox.RunBoidShim([]string{
		"task", "notify", "task-xyz",
		"--message", "Info only",
	})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}

	req := <-reqCh
	if req.Boid.Ask != "" {
		t.Errorf("ask = %q, want empty for normal notify", req.Boid.Ask)
	}
}

func TestRunBoidShim_TaskAnswer_SendsTypedRequest(t *testing.T) {
	sockPath, reqCh := newFakeBrokerSingle(t)
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-answer")

	resp, err := sandbox.RunBoidShim([]string{
		"task", "answer",
		"--task", "task-abc",
		"--question-id", "q-999",
		"--answer", "yes",
	})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskAnswer {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskAnswer)
	}
	if req.Boid.TaskID != "task-abc" {
		t.Errorf("task_id = %q, want task-abc", req.Boid.TaskID)
	}
	if req.Boid.QuestionID != "q-999" {
		t.Errorf("question_id = %q, want q-999", req.Boid.QuestionID)
	}
	if req.Boid.Answer != "yes" {
		t.Errorf("answer = %q, want yes", req.Boid.Answer)
	}
}

func TestRunBoidShim_TaskAnswer_MissingTaskID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	if _, err := sandbox.RunBoidShim([]string{"task", "answer", "--question-id", "q-1", "--answer", "yes"}); err == nil {
		t.Fatal("expected error when --task is missing")
	}
}

func TestRunBoidShim_TaskAnswer_MissingQuestionID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	if _, err := sandbox.RunBoidShim([]string{"task", "answer", "--task", "t-1", "--answer", "yes"}); err == nil {
		t.Fatal("expected error when --question-id is missing")
	}
}

func TestRunBoidShim_TaskAnswer_MissingAnswer(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	if _, err := sandbox.RunBoidShim([]string{"task", "answer", "--task", "t-1", "--question-id", "q-1"}); err == nil {
		t.Fatal("expected error when --answer is missing")
	}
}

// host 側 cmd/job.go runJobDone が --output-file の missing を silent skip
// するのに対し、shim 側だけ ENOENT を fatal にしていると
// agent が /exit で終了して payload_patch.* / /tmp/boid-output が無いケースで
// "exited without boid job done" 相当の失敗になり、対称性を欠く。
func TestRunBoidShim_JobDoneSilentSkipsMissingOutputFile(t *testing.T) {
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

	missingPath := filepath.Join(dir, "does-not-exist.txt")

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-123")

	resp, err := sandbox.RunBoidShim([]string{"job", "done", "job-1", "--exit-code", "0", "--output-file", missingPath})
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
	if req.Boid.Op != sandbox.BoidOpJobDone {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpJobDone)
	}
	if req.Boid.Output != "" {
		t.Fatalf("output = %q, want empty (missing file should be silent)", req.Boid.Output)
	}
}

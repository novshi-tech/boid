package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

func newTaskCreateCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := taskCreateCmd
	cmd.ResetFlags()
	cmd.Flags().StringP("file", "f", "", "YAML file to read task spec from (default: stdin)")
	return cmd
}

func TestRunTaskCreate_PassesBaseBranchFromStdin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "create-base-branch-proj", "Create Base Branch Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	input := `project_id: create-base-branch-proj
title: child task with base branch
behavior: dev
base_branch: feature/BGO-170
`
	cmd := newTaskCreateCmd(t)
	cmd.SetIn(strings.NewReader(input))
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskCreate(cmd, nil); err != nil {
		t.Fatalf("runTaskCreate() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "task created:") {
		t.Fatalf("output should contain 'task created:', got %q", got)
	}

	// Recover task ID from stdout: "task created: <id> (<status>)"
	parts := strings.Split(strings.TrimSpace(got), " ")
	if len(parts) < 3 {
		t.Fatalf("unexpected output format: %q", got)
	}
	taskID := parts[2]

	var task orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.BaseBranch != "feature/BGO-170" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "feature/BGO-170")
	}
}

func TestRunTaskCreate_PassesBaseBranchFromJSONStdin(t *testing.T) {
	// create-subtasks.py が json.dumps した spec を stdin に流す経路を再現する。
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "create-base-branch-json-proj", "Create Base Branch JSON Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	input := `{"project_id":"create-base-branch-json-proj","title":"child","behavior":"dev","base_branch":"feature/X"}`
	cmd := newTaskCreateCmd(t)
	cmd.SetIn(strings.NewReader(input))
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskCreate(cmd, nil); err != nil {
		t.Fatalf("runTaskCreate() error = %v", err)
	}

	parts := strings.Split(strings.TrimSpace(out.String()), " ")
	if len(parts) < 3 {
		t.Fatalf("unexpected output format: %q", out.String())
	}
	taskID := parts[2]

	var task orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.BaseBranch != "feature/X" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "feature/X")
	}
}

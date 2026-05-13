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

// TestRunTaskCreate_DropsDeprecatedBaseBranchFromStdin covers Phase 2-3.
// Legacy YAML specs with `base_branch:` still parse without error (the key
// is stripped + warned), and the created task picks up its base_branch from
// the behavior-level template instead.
func TestRunTaskCreate_DropsDeprecatedBaseBranchFromStdin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "create-base-branch-proj", "Create Base Branch Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	input := `project_id: create-base-branch-proj
title: child task with deprecated base_branch
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

	parts := strings.Split(strings.TrimSpace(got), " ")
	if len(parts) < 3 {
		t.Fatalf("unexpected output format: %q", got)
	}
	taskID := parts[2]

	var task orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.BaseBranch == "feature/BGO-170" {
		t.Errorf("BaseBranch = %q, want value from behavior (deprecated task-row override must be dropped)", task.BaseBranch)
	}
}

// TestRunTaskCreate_DropsDeprecatedBaseBranchFromJSONStdin: same as above
// for the JSON-on-stdin entry point (agent / gate scripts).
func TestRunTaskCreate_DropsDeprecatedBaseBranchFromJSONStdin(t *testing.T) {
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
	if task.BaseBranch == "feature/X" {
		t.Errorf("BaseBranch = %q, want value from behavior (deprecated task-row override must be dropped)", task.BaseBranch)
	}
}

func TestRunTaskCreate_DefaultsParentIDFromBoidTaskIDEnv(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "parent-id-default-proj", "Parent ID Default Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("BOID_TASK_ID", "parent-xyz")

	input := `project_id: parent-id-default-proj
title: child task without explicit parent_id
behavior: dev
`
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
	if task.ParentID != "parent-xyz" {
		t.Errorf("ParentID = %q, want %q", task.ParentID, "parent-xyz")
	}
}

func TestRunTaskCreate_ExplicitParentIDOverridesEnv(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "parent-id-override-proj", "Parent ID Override Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("BOID_TASK_ID", "env-parent")

	input := `project_id: parent-id-override-proj
title: child task with explicit parent_id
behavior: dev
parent_id: foo
`
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
	if task.ParentID != "foo" {
		t.Errorf("ParentID = %q, want %q", task.ParentID, "foo")
	}
}

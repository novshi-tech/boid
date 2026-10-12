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
	if err := testutil.SeedProjectFixture(t, ts, dir, nil); err != nil {
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
	if task.Exec == nil {
		t.Fatal("task.Exec is nil, want an execution task")
	}
	if task.Exec.BaseBranch == "feature/BGO-170" {
		t.Errorf("BaseBranch = %q, want value from behavior (deprecated task-row override must be dropped)", task.Exec.BaseBranch)
	}
}

// TestRunTaskCreate_DropsDeprecatedBaseBranchFromJSONStdin: same as above
// for the JSON-on-stdin entry point (agent scripts).
func TestRunTaskCreate_DropsDeprecatedBaseBranchFromJSONStdin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "create-base-branch-json-proj", "Create Base Branch JSON Project")
	if err := testutil.SeedProjectFixture(t, ts, dir, nil); err != nil {
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
	if task.Exec == nil {
		t.Fatal("task.Exec is nil, want an execution task")
	}
	if task.Exec.BaseBranch == "feature/X" {
		t.Errorf("BaseBranch = %q, want value from behavior (deprecated task-row override must be dropped)", task.Exec.BaseBranch)
	}
}

func TestRunTaskCreate_DefaultsParentIDFromBoidTaskIDEnv(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "parent-id-default-proj", "Parent ID Default Project")
	if err := testutil.SeedProjectFixture(t, ts, dir, nil); err != nil {
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

// TestRunTaskCreate_SentinelRootParentID verifies that parent_id:"-" in the
// YAML spec skips BOID_TASK_ID auto-populate and creates a root task
// (stored ParentID is empty), even when BOID_TASK_ID is set in the environment.
func TestRunTaskCreate_SentinelRootParentID(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "sentinel-parent-proj", "Sentinel Parent Project")
	if err := testutil.SeedProjectFixture(t, ts, dir, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("BOID_TASK_ID", "some-parent-task")

	input := `project_id: sentinel-parent-proj
title: root task via sentinel
behavior: dev
parent_id: "-"
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
	if task.ParentID != "" {
		t.Errorf("ParentID = %q, want empty (sentinel must skip auto-populate)", task.ParentID)
	}
}

func TestRunTaskCreate_ExplicitParentIDOverridesEnv(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "parent-id-override-proj", "Parent ID Override Project")
	if err := testutil.SeedProjectFixture(t, ts, dir, nil); err != nil {
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

func TestRunTaskCreateWorkspaceOwnership(t *testing.T) {
	ts := testutil.NewTestServer(t)
	dir := writeImportTestProject(t, "workspace-child-project", "Child Project")
	if err := testutil.SeedProjectFixture(t, ts, dir, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("BOID_TASK_ID", "")
	for _, tc := range []struct {
		parentProject string
		noProject     bool
		wantProject   string
	}{
		{"workspace-child-project", false, "workspace-child-project"},
		{"workspace-child-project", true, ""},
		{"", false, ""},
	} {
		t.Setenv("BOID_PROJECT_ID", tc.parentProject)
		t.Setenv("BOID_WORKSPACE_SLUG", "default")
		cmd := newTaskCreateCmd(t)
		cmd.Flags().Bool("no-project", tc.noProject, "")
		cmd.Flags().String("workspace", "", "")
		cmd.SetIn(strings.NewReader("title: workspace child\ninitial_status: parked\n"))
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := runTaskCreate(cmd, nil); err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(out.String())
		if len(fields) < 3 {
			t.Fatal(out.String())
		}
		var task orchestrator.Task
		if err := ts.Client.Do("GET", "/api/tasks/"+fields[2], nil, &task); err != nil {
			t.Fatal(err)
		}
		if task.ProjectID != tc.wantProject || task.WorkspaceID != "default" {
			t.Fatalf("%+v: %+v", tc, task)
		}
	}
}

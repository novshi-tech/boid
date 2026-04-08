package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunTaskDuplicate_OutputsNewTaskID(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "dup-cmd-proj", "Dup Cmd Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// ソースタスクを作成
	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "dup-cmd-proj",
		"title":      "CLI Source Task",
		"behavior":   "dev",
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := taskDuplicateCmd
	cmd.SetOut(&out)
	cmd.ResetFlags()
	cmd.Flags().Bool("auto-start", false, "auto-start")

	if err := runTaskDuplicate(cmd, []string{source.ID}); err != nil {
		t.Fatalf("runTaskDuplicate() error = %v", err)
	}

	got := strings.TrimSpace(out.String())
	if got == "" {
		t.Fatal("output should contain new task ID, got empty string")
	}
	if got == source.ID {
		t.Errorf("output ID %q should differ from source ID", got)
	}
}

func TestRunTaskDuplicate_NotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := taskDuplicateCmd
	cmd.ResetFlags()
	cmd.Flags().Bool("auto-start", false, "auto-start")

	err := runTaskDuplicate(cmd, []string{"nonexistent-id"})
	if err == nil {
		t.Fatal("runTaskDuplicate() expected error for nonexistent task, got nil")
	}
}

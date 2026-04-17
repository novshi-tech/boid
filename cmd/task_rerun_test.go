package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

func newTaskRerunCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := taskRerunCmd
	cmd.ResetFlags()
	cmd.Flags().Bool("auto-start", false, "auto-start")
	cmd.Flags().String("instructions-file", "", "instructions file")
	return cmd
}

// abortTaskViaAction flips a task to aborted status through the public abort
// action so the rerun API accepts it afterwards.
func abortTaskViaAction(t *testing.T, ts *testutil.TestServer, taskID string) {
	t.Helper()
	if err := ts.Client.Do("POST", "/api/tasks/"+taskID+"/actions", map[string]any{"type": "abort"}, nil); err != nil {
		t.Fatalf("abort task %s: %v", taskID, err)
	}
}

func TestRunTaskRerun_SendsInstructionsOverride(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "rerun-instr-proj", "Rerun Instructions Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-instr-proj",
		"title":      "rerun task",
		"behavior":   "dev",
		"instructions": map[string]any{
			"main": map[string]any{
				"type":     "execution",
				"consumer": "claude-code",
				"model":    "sonnet-4-6",
				"message":  "initial",
			},
		},
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	abortTaskViaAction(t, ts, task.ID)

	instructionsYAML := `main:
  type: execution
  consumer: claude-code
  model: opus-4-7
  message: "retry with opus"
`
	tmpFile := filepath.Join(t.TempDir(), "instructions.yaml")
	if err := os.WriteFile(tmpFile, []byte(instructionsYAML), 0o644); err != nil {
		t.Fatalf("write instructions file: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskRerunCmd(t)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("instructions-file", tmpFile); err != nil {
		t.Fatalf("set --instructions-file: %v", err)
	}

	if err := runTaskRerun(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskRerun() error = %v", err)
	}

	var reran orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &reran); err != nil {
		t.Fatalf("get reran task: %v", err)
	}
	if reran.Status != orchestrator.TaskStatusPending {
		t.Errorf("status = %q, want pending", reran.Status)
	}
	got, ok := reran.Instructions["main"]
	if !ok {
		t.Fatalf("instructions.main not set; got: %#v", reran.Instructions)
	}
	if got.Model != "opus-4-7" {
		t.Errorf("instructions.main.model = %q, want opus-4-7", got.Model)
	}
	if got.Message != "retry with opus" {
		t.Errorf("instructions.main.message = %q, want %q", got.Message, "retry with opus")
	}
}

func TestRunTaskRerun_NoInstructionsFlagPreservesExisting(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "rerun-noinstr-proj", "Rerun No Instructions Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-noinstr-proj",
		"title":      "rerun task",
		"behavior":   "dev",
		"instructions": map[string]any{
			"main": map[string]any{
				"type":     "execution",
				"consumer": "claude-code",
				"message":  "keep me",
			},
		},
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	abortTaskViaAction(t, ts, task.ID)

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskRerunCmd(t)
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskRerun(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskRerun() error = %v", err)
	}

	var reran orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &reran); err != nil {
		t.Fatalf("get reran task: %v", err)
	}
	got, ok := reran.Instructions["main"]
	if !ok {
		t.Fatalf("instructions.main should be preserved; got: %#v", reran.Instructions)
	}
	if got.Message != "keep me" {
		t.Errorf("instructions.main.message = %q, want %q", got.Message, "keep me")
	}
}

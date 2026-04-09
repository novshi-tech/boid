package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

func newTaskUpdateCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := taskUpdateCmd
	cmd.ResetFlags()
	cmd.Flags().String("title", "", "title")
	cmd.Flags().String("description", "", "description")
	cmd.Flags().String("payload-file", "", "payload file")
	return cmd
}

func TestRunTaskUpdate_UpdatesTitle(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-title-proj", "Update Title Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-title-proj",
		"title":      "original title",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := newTaskUpdateCmd(t)
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("title", "new title"); err != nil {
		t.Fatalf("set --title: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.Title != "new title" {
		t.Errorf("Title = %q, want %q", updated.Title, "new title")
	}
}

func TestRunTaskUpdate_UpdatesPayloadFromFile(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-payload-proj", "Update Payload Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-payload-proj",
		"title":      "payload task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	payload := map[string]any{"result": "merged"}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	tmpFile := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(tmpFile, payloadBytes, 0o644); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := newTaskUpdateCmd(t)
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("payload-file", tmpFile); err != nil {
		t.Fatalf("set --payload-file: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, task.ID) {
		t.Errorf("output %q should contain task ID %q", got, task.ID)
	}
}

func TestRunTaskUpdate_NoFlagsReturnsError(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-noflag-proj", "Update NoFlag Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-noflag-proj",
		"title":      "noflag task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskUpdateCmd(t)
	err := runTaskUpdate(cmd, []string{task.ID})
	if err == nil {
		t.Fatal("runTaskUpdate() expected error when no flags specified, got nil")
	}
}

func TestRunTaskUpdate_NotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskUpdateCmd(t)
	if err := cmd.Flags().Set("title", "new title"); err != nil {
		t.Fatalf("set --title: %v", err)
	}

	err := runTaskUpdate(cmd, []string{"nonexistent-id"})
	if err == nil {
		t.Fatal("runTaskUpdate() expected error for nonexistent task, got nil")
	}
}

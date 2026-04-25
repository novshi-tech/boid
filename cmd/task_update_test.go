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
	cmd.Flags().StringP("patch-file", "f", "", "patch file")
	cmd.Flags().String("payload-file", "", "payload file")
	cmd.Flags().String("instructions-file", "", "instructions file")
	return cmd
}

// writePatch writes a YAML/JSON patch to a temp file and returns the path.
func writePatch(t *testing.T, content string) string {
	t.Helper()
	tmpFile := filepath.Join(t.TempDir(), "patch.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write patch file: %v", err)
	}
	return tmpFile
}

func TestRunTaskUpdate_UpdatesTitleViaPatch(t *testing.T) {
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

	patchPath := writePatch(t, `title: new title`)

	var out bytes.Buffer
	cmd := newTaskUpdateCmd(t)
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("patch-file", patchPath); err != nil {
		t.Fatalf("set --patch-file: %v", err)
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

func TestRunTaskUpdate_UpdatesInstructionsFromFile(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-instr-proj", "Update Instructions Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-instr-proj",
		"title":      "instruction task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	instructionsYAML := `main:
  type: execution
  consumer: claude-code
  message: "do the thing"
  model: opus-4-7
`
	tmpFile := filepath.Join(t.TempDir(), "instructions.yaml")
	if err := os.WriteFile(tmpFile, []byte(instructionsYAML), 0o644); err != nil {
		t.Fatalf("write instructions file: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := newTaskUpdateCmd(t)
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("instructions-file", tmpFile); err != nil {
		t.Fatalf("set --instructions-file: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	got, ok := updated.Instructions["main"]
	if !ok {
		t.Fatalf("instructions.main not set; got: %#v", updated.Instructions)
	}
	if got.Model != "opus-4-7" {
		t.Errorf("instructions.main.model = %q, want opus-4-7", got.Model)
	}
	if got.Message != "do the thing" {
		t.Errorf("instructions.main.message = %q, want %q", got.Message, "do the thing")
	}
}

func TestRunTaskUpdate_InstructionsFromStdin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-instr-stdin-proj", "Update Instructions Stdin Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-instr-stdin-proj",
		"title":      "stdin instruction task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskUpdateCmd(t)
	cmd.SetIn(strings.NewReader(`{"reviewer":{"type":"verification","consumer":"codex","message":"review"}}`))
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("instructions-file", "-"); err != nil {
		t.Fatalf("set --instructions-file: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if _, ok := updated.Instructions["reviewer"]; !ok {
		t.Fatalf("instructions.reviewer not set; got: %#v", updated.Instructions)
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

	patchPath := writePatch(t, `title: new title`)

	cmd := newTaskUpdateCmd(t)
	if err := cmd.Flags().Set("patch-file", patchPath); err != nil {
		t.Fatalf("set --patch-file: %v", err)
	}

	err := runTaskUpdate(cmd, []string{"nonexistent-id"})
	if err == nil {
		t.Fatal("runTaskUpdate() expected error for nonexistent task, got nil")
	}
}

func TestRunTaskUpdate_PatchFileUpdatesWorktreeAndBaseBranch(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-patch-proj", "Update Patch Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id":  "update-patch-proj",
		"title":       "patch target",
		"behavior":    "dev",
		"worktree":    true,
		"base_branch": "main",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if !task.Worktree {
		t.Fatalf("setup precondition: Worktree = false, want true")
	}

	patchPath := writePatch(t, `worktree: false
base_branch: develop
`)

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskUpdateCmd(t)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("patch-file", patchPath); err != nil {
		t.Fatalf("set --patch-file: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.Worktree {
		t.Errorf("Worktree = true, want false")
	}
	if updated.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want %q", updated.BaseBranch, "develop")
	}
}

func TestRunTaskUpdate_PatchFileFromStdin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-patch-stdin-proj", "Update Patch Stdin Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-patch-stdin-proj",
		"title":      "stdin patch target",
		"behavior":   "dev",
		"worktree":   true,
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := newTaskUpdateCmd(t)
	cmd.SetIn(strings.NewReader(`{"worktree": false}`))
	if err := cmd.Flags().Set("patch-file", "-"); err != nil {
		t.Fatalf("set --patch-file: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.Worktree {
		t.Errorf("Worktree = true, want false")
	}
}

func TestRunTaskUpdate_PatchClearsDependsOnPayload(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "update-dop-proj", "Update DependsOnPayload Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// 依存先と本体タスクを作成
	var dep orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "update-dop-proj",
		"title":      "dependency",
		"behavior":   "dev",
	}, &dep); err != nil {
		t.Fatalf("create dep: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id":         "update-dop-proj",
		"title":              "main task",
		"behavior":           "dev",
		"depends_on":         []string{dep.ID},
		"depends_on_payload": "artifact.auto-merge.merged",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	patchPath := writePatch(t, `depends_on_payload: ""`)

	cmd := newTaskUpdateCmd(t)
	if err := cmd.Flags().Set("patch-file", patchPath); err != nil {
		t.Fatalf("set --patch-file: %v", err)
	}

	if err := runTaskUpdate(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskUpdate() error = %v", err)
	}

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.DependsOnPayload != "" {
		t.Errorf("DependsOnPayload = %q, want empty", updated.DependsOnPayload)
	}
}

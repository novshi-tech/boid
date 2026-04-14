package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// --- helpers ---

func createInspectProject(t *testing.T, ts *testutil.TestServer) {
	t.Helper()
	dir := writeInspectTestProject(t, "inspect-proj", "Inspect Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
}

// writeInspectTestProject creates a project with dev and review behaviors.
func writeInspectTestProject(t *testing.T, id, name string) string {
	t.Helper()
	dir := t.TempDir()
	boidDir := dir + "/.boid"
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	content := "id: " + id + "\nname: " + name + "\ntask_behaviors:\n  dev:\n    name: development\n  review:\n    name: review\n"
	if err := os.WriteFile(boidDir+"/project.yaml", []byte(content), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	return dir
}

func createTaskWithPayload(t *testing.T, ts *testutil.TestServer, title string, payload map[string]any) orchestrator.Task {
	t.Helper()
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      title,
		"behavior":   "dev",
		"payload":    json.RawMessage(payloadJSON),
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// --- boid task findings tests ---

func TestRunTaskFindings_ShowsOpenFindings(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	payload := map[string]any{
		"verification": map[string]any{
			"gate-1": map[string]any{
				"source_state": "verifying",
				"findings": []any{
					map[string]any{"message": "conflict detected", "status": "open"},
					map[string]any{"message": "all good", "status": "resolved"},
				},
			},
		},
	}
	task := createTaskWithPayload(t, ts, "Findings Task", payload)

	cmd := taskFindingsCmd
	cmd.ResetFlags()
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("status", "", "")
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskFindings(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskFindings() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "conflict detected") {
		t.Errorf("output missing open finding: %q", got)
	}
	if !strings.Contains(got, "gate-1") {
		t.Errorf("output missing gate name: %q", got)
	}
	if !strings.Contains(got, "open") {
		t.Errorf("output missing status: %q", got)
	}
	// resolved finding should NOT appear in default (open-only) mode
	if strings.Contains(got, "all good") {
		t.Errorf("resolved finding should not appear in default mode: %q", got)
	}
}

func TestRunTaskFindings_AllFlag(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	payload := map[string]any{
		"verification": map[string]any{
			"gate-1": map[string]any{
				"source_state": "verifying",
				"findings": []any{
					map[string]any{"message": "conflict detected", "status": "open"},
					map[string]any{"message": "all good", "status": "resolved"},
				},
			},
		},
	}
	task := createTaskWithPayload(t, ts, "All Findings Task", payload)

	cmd := taskFindingsCmd
	cmd.ResetFlags()
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("status", "", "")
	if err := cmd.Flags().Set("all", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskFindings(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskFindings() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "conflict detected") {
		t.Errorf("output missing open finding: %q", got)
	}
	if !strings.Contains(got, "all good") {
		t.Errorf("--all: output missing resolved finding: %q", got)
	}
}

func TestRunTaskFindings_StatusFilter(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	payload := map[string]any{
		"verification": map[string]any{
			"gate-1": map[string]any{
				"findings": []any{
					map[string]any{"message": "open-msg", "status": "open"},
					map[string]any{"message": "resolved-msg", "status": "resolved"},
				},
			},
		},
	}
	task := createTaskWithPayload(t, ts, "Status Filter Task", payload)

	cmd := taskFindingsCmd
	cmd.ResetFlags()
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("status", "", "")
	if err := cmd.Flags().Set("status", "resolved"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskFindings(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskFindings() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "resolved-msg") {
		t.Errorf("--status resolved: output missing resolved finding: %q", got)
	}
	if strings.Contains(got, "open-msg") {
		t.Errorf("--status resolved: open finding should not appear: %q", got)
	}
}

func TestRunTaskFindings_NoFindings(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	task := createTaskWithPayload(t, ts, "No Findings Task", map[string]any{})

	cmd := taskFindingsCmd
	cmd.ResetFlags()
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().String("status", "", "")
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskFindings(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskFindings() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "no findings") {
		t.Errorf("expected 'no findings', got: %q", got)
	}
}

// --- boid task artifacts tests ---

func TestRunTaskArtifacts_ShowsYAML(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	payload := map[string]any{
		"artifact": map[string]any{
			"commit":        "abc1234",
			"files_changed": []string{"main.go", "main_test.go"},
		},
	}
	task := createTaskWithPayload(t, ts, "Artifacts Task", payload)

	cmd := taskArtifactsCmd
	cmd.ResetFlags()
	cmd.Flags().String("field", "", "")
	cmd.Flags().String("output-file", "", "")
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskArtifacts(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskArtifacts() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "commit") {
		t.Errorf("output missing 'commit': %q", got)
	}
	if !strings.Contains(got, "abc1234") {
		t.Errorf("output missing commit value: %q", got)
	}
	if !strings.Contains(got, "files_changed") {
		t.Errorf("output missing files_changed: %q", got)
	}
}

func TestRunTaskArtifacts_WithField(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	payload := map[string]any{
		"artifact": map[string]any{
			"commit": "deadbeef",
			"other":  "ignored",
		},
	}
	task := createTaskWithPayload(t, ts, "Artifacts Field Task", payload)

	cmd := taskArtifactsCmd
	cmd.ResetFlags()
	cmd.Flags().String("field", "", "")
	cmd.Flags().String("output-file", "", "")
	if err := cmd.Flags().Set("field", "commit"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskArtifacts(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskArtifacts() error = %v", err)
	}

	got := strings.TrimSpace(out.String())
	if got != "deadbeef" {
		t.Errorf("--field commit: got %q, want %q", got, "deadbeef")
	}
}

func TestRunTaskArtifacts_NoArtifact(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	task := createTaskWithPayload(t, ts, "No Artifact Task", map[string]any{})

	cmd := taskArtifactsCmd
	cmd.ResetFlags()
	cmd.Flags().String("field", "", "")
	cmd.Flags().String("output-file", "", "")
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskArtifacts(cmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskArtifacts() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "no artifact") {
		t.Errorf("expected 'no artifact', got: %q", got)
	}
}

// --- boid task tree tests ---

func TestRunTaskTree_AllTasks(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	// root task (no parent, no depends_on)
	var root orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Root Task",
		"behavior":   "dev",
	}, &root); err != nil {
		t.Fatalf("create root: %v", err)
	}

	// child of root
	var child orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Child Task",
		"behavior":   "dev",
		"parent_id":  root.ID,
	}, &child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	cmd := taskTreeCmd
	cmd.ResetFlags()
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskTree(cmd, []string{}); err != nil {
		t.Fatalf("runTaskTree() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Root Task") {
		t.Errorf("output missing Root Task: %q", got)
	}
	if !strings.Contains(got, "Child Task") {
		t.Errorf("output missing Child Task: %q", got)
	}
}

func TestRunTaskTree_Subtree(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	// root1 with a child
	var root1 orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Root1",
		"behavior":   "dev",
	}, &root1); err != nil {
		t.Fatalf("create root1: %v", err)
	}
	var child1 orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Child1",
		"behavior":   "dev",
		"parent_id":  root1.ID,
	}, &child1); err != nil {
		t.Fatalf("create child1: %v", err)
	}

	// root2 with a child (should NOT appear in subtree of root1)
	var root2 orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Root2",
		"behavior":   "dev",
	}, &root2); err != nil {
		t.Fatalf("create root2: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Child2",
		"behavior":   "dev",
		"parent_id":  root2.ID,
	}, nil); err != nil {
		t.Fatalf("create child2: %v", err)
	}

	cmd := taskTreeCmd
	cmd.ResetFlags()
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskTree(cmd, []string{root1.ID}); err != nil {
		t.Fatalf("runTaskTree() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Root1") {
		t.Errorf("output missing Root1: %q", got)
	}
	if !strings.Contains(got, "Child1") {
		t.Errorf("output missing Child1: %q", got)
	}
	if strings.Contains(got, "Root2") {
		t.Errorf("output should not contain Root2: %q", got)
	}
	if strings.Contains(got, "Child2") {
		t.Errorf("output should not contain Child2: %q", got)
	}
}

// --- boid task list filter tests ---

func TestRunTaskList_BehaviorFilter(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Dev Task",
		"behavior":   "dev",
	}, nil); err != nil {
		t.Fatalf("create dev task: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Review Task",
		"behavior":   "review",
	}, nil); err != nil {
		t.Fatalf("create review task: %v", err)
	}

	cmd := taskListCmd
	cmd.ResetFlags()
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("workspace", "", "")
	cmd.Flags().String("behavior", "", "")
	cmd.Flags().Bool("has-depends-on", false, "")
	cmd.Flags().Bool("no-depends-on", false, "")
	if err := cmd.Flags().Set("behavior", "dev"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskList(cmd, []string{}); err != nil {
		t.Fatalf("runTaskList() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Dev Task") {
		t.Errorf("output missing Dev Task: %q", got)
	}
	if strings.Contains(got, "Review Task") {
		t.Errorf("output should not contain Review Task when filtering by dev: %q", got)
	}
}

func TestRunTaskList_HasDependsOn(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	// task without depends_on
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Standalone Task",
		"behavior":   "dev",
	}, nil); err != nil {
		t.Fatalf("create standalone: %v", err)
	}

	// task with depends_on
	var dep orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Dep Source",
		"behavior":   "dev",
	}, &dep); err != nil {
		t.Fatalf("create dep source: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Dependent Task",
		"behavior":   "dev",
		"depends_on": []string{dep.ID},
	}, nil); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	cmd := taskListCmd
	cmd.ResetFlags()
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("workspace", "", "")
	cmd.Flags().String("behavior", "", "")
	cmd.Flags().Bool("has-depends-on", false, "")
	cmd.Flags().Bool("no-depends-on", false, "")
	if err := cmd.Flags().Set("has-depends-on", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskList(cmd, []string{}); err != nil {
		t.Fatalf("runTaskList() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Dependent Task") {
		t.Errorf("output missing Dependent Task: %q", got)
	}
	if strings.Contains(got, "Standalone Task") {
		t.Errorf("output should not contain Standalone Task: %q", got)
	}
}

func TestRunTaskList_NoDependsOn(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createInspectProject(t, ts)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	// task without depends_on
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Standalone Task",
		"behavior":   "dev",
	}, nil); err != nil {
		t.Fatalf("create standalone: %v", err)
	}

	// task with depends_on
	var dep orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Dep Source",
		"behavior":   "dev",
	}, &dep); err != nil {
		t.Fatalf("create dep source: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "inspect-proj",
		"title":      "Dependent Task",
		"behavior":   "dev",
		"depends_on": []string{dep.ID},
	}, nil); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	cmd := taskListCmd
	cmd.ResetFlags()
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("workspace", "", "")
	cmd.Flags().String("behavior", "", "")
	cmd.Flags().Bool("has-depends-on", false, "")
	cmd.Flags().Bool("no-depends-on", false, "")
	if err := cmd.Flags().Set("no-depends-on", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runTaskList(cmd, []string{}); err != nil {
		t.Fatalf("runTaskList() error = %v", err)
	}

	got := out.String()
	if strings.Contains(got, "Dependent Task") {
		t.Errorf("output should not contain Dependent Task: %q", got)
	}
	if !strings.Contains(got, "Standalone Task") {
		t.Errorf("output missing Standalone Task: %q", got)
	}
}

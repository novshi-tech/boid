package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// TestTaskHandler_Create_SentinelRootParentID verifies that POSTing
// parent_id:"-" to the HTTP API normalises the sentinel to an empty string
// and creates a root task, not a child task.
func TestTaskHandler_Create_SentinelRootParentID(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := t.TempDir()
	testutil.InitGitRepoWithOrigin(t, dir)
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid: %v", err)
	}
	yaml := "id: sentinel-proj\nname: Sentinel Test\ntask_behaviors:\n  planning:\n    name: Planning\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Establish a "parent" task so parent_id:"-" is not confused with it.
	var parent orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "sentinel-proj",
		"title":      "parent task",
		"behavior":   "planning",
	}, &parent); err != nil {
		t.Fatalf("create parent task: %v", err)
	}

	// Create a task with sentinel parent_id:"-" — must become a root task.
	var root orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "sentinel-proj",
		"title":      "root via sentinel",
		"behavior":   "planning",
		"parent_id":  "-",
	}, &root); err != nil {
		t.Fatalf("create root task via sentinel: %v", err)
	}

	if root.ParentID != "" {
		t.Errorf("ParentID = %q, want empty (sentinel must produce root task)", root.ParentID)
	}
	if root.ID == parent.ID {
		t.Errorf("sentinel task got same ID as parent (%s)", root.ID)
	}
}

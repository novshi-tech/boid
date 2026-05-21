package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func createProjectWithBehavior(t *testing.T, ts *testutil.TestServer, id, name string) {
	t.Helper()
	dir := setupTestProject(t, id, name, false)
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}
}

func TestImportTasks_JSONArray(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "test-import", "Test Import")

	tasks := []api.CreateTaskRequest{
		{ProjectID: "test-import", Title: "Task 1", Behavior: "planning", RemoteID: "PROJ-1"},
		{ProjectID: "test-import", Title: "Task 2", Behavior: "planning", RemoteID: "PROJ-2"},
	}
	body, _ := json.Marshal(tasks)

	var result api.ImportResult
	if err := ts.Client.DoWithContentType("POST", "/api/tasks/import", "application/json", body, &result); err != nil {
		t.Fatalf("POST /api/tasks/import: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("Created = %d, want 2", result.Created)
	}
	if result.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0", result.Skipped)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want empty", result.Errors)
	}
}

func TestImportTasks_NDJSON(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "test-import-ndjson", "Test Import NDJSON")

	lines := strings.Join([]string{
		`{"project_id":"test-import-ndjson","title":"Task A","behavior":"planning","remote_id":"PROJ-A"}`,
		`{"project_id":"test-import-ndjson","title":"Task B","behavior":"planning","remote_id":"PROJ-B"}`,
	}, "\n")

	var result api.ImportResult
	if err := ts.Client.DoWithContentType("POST", "/api/tasks/import", "application/x-ndjson", []byte(lines), &result); err != nil {
		t.Fatalf("POST /api/tasks/import: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("Created = %d, want 2", result.Created)
	}
}

func TestImportTasks_SkipsDuplicate(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "test-import-dup", "Test Import Dup")

	tasks := []api.CreateTaskRequest{
		{ProjectID: "test-import-dup", Title: "Task 1", Behavior: "planning", RemoteID: "PROJ-1"},
	}
	body, _ := json.Marshal(tasks)

	var r1 api.ImportResult
	if err := ts.Client.DoWithContentType("POST", "/api/tasks/import", "application/json", body, &r1); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if r1.Created != 1 {
		t.Fatalf("first import Created = %d, want 1", r1.Created)
	}

	var r2 api.ImportResult
	if err := ts.Client.DoWithContentType("POST", "/api/tasks/import", "application/json", body, &r2); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if r2.Created != 0 {
		t.Fatalf("second import Created = %d, want 0", r2.Created)
	}
	if r2.Skipped != 1 {
		t.Fatalf("second import Skipped = %d, want 1", r2.Skipped)
	}
}

func TestImportTasks_ValidationError_BothIDsEmpty(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "test-import-val", "Test Import Val")

	tasks := []api.CreateTaskRequest{
		{ProjectID: "test-import-val", Title: "No Remote", Behavior: "planning"},
	}
	body, _ := json.Marshal(tasks)

	var result api.ImportResult
	if err := ts.Client.DoWithContentType("POST", "/api/tasks/import", "application/json", body, &result); err != nil {
		t.Fatalf("POST /api/tasks/import: %v", err)
	}
	if result.Created != 0 {
		t.Fatalf("Created = %d, want 0", result.Created)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if result.Errors[0].Line != 1 {
		t.Fatalf("Errors[0].Line = %d, want 1", result.Errors[0].Line)
	}
}

func TestImportTasks_InvalidJSON(t *testing.T) {
	ts := testutil.NewTestServer(t)

	if err := ts.Client.DoWithContentType("POST", "/api/tasks/import", "application/json", []byte(`not-json`), nil); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

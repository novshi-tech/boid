package api_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestProjectAPI_PermissionRegistrationUpdateAndDisplay(t *testing.T) {
	for _, perm := range []string{"", "fetch", "push", "invalid"} {
		t.Run("perm="+perm, func(t *testing.T) {
			ts := testutil.NewTestServer(t)
			dir := setupTestProject(t, "project", "Project", false)
			var p orchestrator.Project
			err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir, "perm": perm}, &p)
			if perm == "invalid" {
				if err == nil {
					t.Fatal("invalid permission accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := perm
			if want == "" {
				want = "push"
			}
			if p.Perm != want {
				t.Fatalf("perm %q, want %q", p.Perm, want)
			}
			if err := ts.Client.Do("PUT", "/api/projects/project/perm", map[string]string{"perm": "fetch"}, &p); err != nil {
				t.Fatal(err)
			}
			if p.Perm != "fetch" {
				t.Fatalf("updated perm %q", p.Perm)
			}
			var projects []orchestrator.Project
			if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, project := range projects {
				if project.ID == p.ID {
					found = true
					if project.Perm != "fetch" {
						t.Fatal(project.Perm)
					}
				}
			}
			if !found {
				t.Fatal("project absent from list")
			}
			if err := ts.Client.Do("GET", "/api/projects/project", nil, &p); err != nil {
				t.Fatal(err)
			}
			if p.Perm != "fetch" {
				t.Fatal(p.Perm)
			}
			if err := ts.Client.Do("PUT", "/api/projects/project/perm", map[string]string{"perm": "invalid"}, nil); err == nil {
				t.Fatal("invalid update accepted")
			}
		})
	}
}

func TestTaskAPI_PrimaryProjectPermission(t *testing.T) {
	ts := testutil.NewTestServer(t)
	p := createProject(t, ts, "project", "Project")
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", api.CreateTaskRequest{ProjectID: p.ID, Title: "allowed", Behavior: "planning"}, &task); err != nil {
		t.Fatal(err)
	}
	// Existing tasks remain editable after their project's permission changes.
	if err := ts.Client.Do("PUT", "/api/projects/project/perm", map[string]string{"perm": "fetch"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.Client.Do("PATCH", "/api/tasks/"+task.ID, api.UpdateTaskRequest{Title: "existing task remains editable", ProjectID: p.ID}, nil); err != nil {
		t.Fatal(err)
	}

	for _, req := range []api.CreateTaskRequest{
		{ProjectID: p.ID, Title: "root", Behavior: "planning"},
		{ProjectID: p.ID, Title: "child", Behavior: "planning", ParentID: task.ID, Ref: "child"},
		{ProjectID: p.ID, Title: "card", InitialStatus: "parked"},
	} {
		if err := ts.Client.Do("POST", "/api/tasks", req, nil); err == nil || !strings.Contains(err.Error(), "perm=fetch") {
			t.Fatalf("expected fetch rejection, got %v for %+v", err, req)
		}
	}
	other := createProject(t, ts, "other", "Other")
	var pending orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", api.CreateTaskRequest{ProjectID: other.ID, Title: "pending", Behavior: "planning"}, &pending); err != nil {
		t.Fatal(err)
	}
	if err := ts.Client.Do("PATCH", "/api/tasks/"+pending.ID, api.UpdateTaskRequest{ProjectID: p.ID}, nil); err == nil || !strings.Contains(err.Error(), "perm=fetch") {
		t.Fatalf("expected fetch reassignment rejection: %v", err)
	}
	if err := ts.Client.Do("GET", "/api/tasks/"+pending.ID, nil, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.ProjectID != other.ID {
		t.Fatalf("failed update changed project to %q", pending.ProjectID)
	}

}

func TestTaskAPI_PermissionChangePreservesCreateRetries(t *testing.T) {
	for _, kind := range []string{"execution", "card"} {
		for _, key := range []string{"ref", "idempotency_key", "ref_miss_with_key"} {
			t.Run(kind+"/"+key, func(t *testing.T) {
				ts := testutil.NewTestServer(t)
				p := createProject(t, ts, "project", "Project")
				req := api.CreateTaskRequest{ProjectID: p.ID, Title: "original", Behavior: "planning"}
				if kind == "card" {
					req.InitialStatus = "parked"
				}
				if key == "ref" {
					req.Ref = "stable-ref"
				} else {
					req.IdempotencyKey = "stable-key"
				}
				var original orchestrator.Task
				if err := ts.Client.Do("POST", "/api/tasks", req, &original); err != nil {
					t.Fatal(err)
				}
				if err := ts.Client.Do("PUT", "/api/projects/project/perm", map[string]string{"perm": "fetch"}, nil); err != nil {
					t.Fatal(err)
				}
				req.Title = "retry"
				if key == "ref_miss_with_key" {
					req.Ref = "new-ref"
				}
				var retry orchestrator.Task
				if err := ts.Client.Do("POST", "/api/tasks", req, &retry); err != nil {
					t.Fatal(err)
				}
				if retry.ID != original.ID || retry.Title != original.Title {
					t.Fatalf("retry = %+v, want original %s", retry, original.ID)
				}
			})
		}
	}
}

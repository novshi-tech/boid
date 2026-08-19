package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// docs/plans/ingestion-identity.md PR-2 (B-2): `boid task resolve-or-capture`
// shim parsing.
//
//	boid task resolve-or-capture <identity> [--title T]
//	    [--description D | --description-file F] [--project-id P]

func TestParseBoidTaskResolveOrCapture(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpTaskResolveOrCapture {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpTaskResolveOrCapture)
	}
	if req.Identity != "jira:X-1" {
		t.Fatalf("identity = %q, want jira:X-1", req.Identity)
	}
	if req.ProjectID != "" || req.Title != "" || req.Description != "" {
		t.Fatalf("req = %+v, want project_id/title/description all empty (not given)", req)
	}
}

func TestParseBoidTaskResolveOrCapture_WithTitleAndDescription(t *testing.T) {
	req, err := parseBoidRequest([]string{
		"task", "resolve-or-capture", "jira:X-1",
		"--title", "something broke",
		"--description", "the body",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Title != "something broke" {
		t.Fatalf("title = %q, want %q", req.Title, "something broke")
	}
	if req.Description != "the body" {
		t.Fatalf("description = %q, want %q", req.Description, "the body")
	}
}

func TestParseBoidTaskResolveOrCapture_DescriptionFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "description.txt")
	if err := os.WriteFile(path, []byte("body from a file\n"), 0o600); err != nil {
		t.Fatalf("write description file: %v", err)
	}

	req, err := parseBoidRequest([]string{
		"task", "resolve-or-capture", "jira:X-1", "--description-file", path,
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Description != "body from a file\n" {
		t.Fatalf("description = %q, want file contents", req.Description)
	}
}

func TestParseBoidTaskResolveOrCapture_WithProjectID(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1", "--project-id", "p1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ProjectID != "p1" {
		t.Fatalf("project id = %q, want p1", req.ProjectID)
	}

	req2, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1", "--project-id=p1"})
	if err != nil {
		t.Fatalf("parse (=form): %v", err)
	}
	if req2.ProjectID != "p1" {
		t.Fatalf("project id (=form) = %q, want p1", req2.ProjectID)
	}
}

func TestParseBoidTaskResolveOrCapture_RequiresIdentity(t *testing.T) {
	cases := map[string][]string{
		"no arguments":     {"task", "resolve-or-capture"},
		"too many args":    {"task", "resolve-or-capture", "jira:X-1", "extra"},
		"unsupported flag": {"task", "resolve-or-capture", "jira:X-1", "--bogus"},
	}
	for name, args := range cases {
		if _, err := parseBoidRequest(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

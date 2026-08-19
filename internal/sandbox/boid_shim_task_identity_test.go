package sandbox

import "testing"

// docs/plans/ingestion-identity.md PR-1 (B-1): `boid task identity
// link/unlink/resolve` shim parsing.
//
//	boid task identity link <identity> <task-id> [--project-id P]
//	boid task identity unlink <identity> [--project-id P]
//	boid task identity resolve <identity> [--project-id P]

func TestParseBoidTaskIdentityLink(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "identity", "link", "jira:X-1", "t1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpTaskIdentityLink {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpTaskIdentityLink)
	}
	if req.Identity != "jira:X-1" || req.TaskID != "t1" {
		t.Fatalf("req = %+v, want identity=jira:X-1 task_id=t1", req)
	}
	if req.ProjectID != "" {
		t.Fatalf("project id = %q, want empty (not given)", req.ProjectID)
	}
}

func TestParseBoidTaskIdentityLink_WithProjectID(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "identity", "link", "jira:X-1", "t1", "--project-id", "p1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ProjectID != "p1" {
		t.Fatalf("project id = %q, want p1", req.ProjectID)
	}

	req2, err := parseBoidRequest([]string{"task", "identity", "link", "jira:X-1", "t1", "--project-id=p1"})
	if err != nil {
		t.Fatalf("parse (=form): %v", err)
	}
	if req2.ProjectID != "p1" {
		t.Fatalf("project id (=form) = %q, want p1", req2.ProjectID)
	}
}

func TestParseBoidTaskIdentityLink_RequiresIdentityAndTaskID(t *testing.T) {
	cases := map[string][]string{
		"no arguments":     {"task", "identity", "link"},
		"identity only":    {"task", "identity", "link", "jira:X-1"},
		"too many args":    {"task", "identity", "link", "jira:X-1", "t1", "t2"},
		"unsupported flag": {"task", "identity", "link", "jira:X-1", "t1", "--bogus"},
	}
	for name, args := range cases {
		if _, err := parseBoidRequest(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

func TestParseBoidTaskIdentityUnlink(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "identity", "unlink", "jira:X-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpTaskIdentityUnlink {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpTaskIdentityUnlink)
	}
	if req.Identity != "jira:X-1" {
		t.Fatalf("identity = %q, want jira:X-1", req.Identity)
	}
	if req.TaskID != "" {
		t.Fatalf("task id = %q, want empty (unlink takes no task id)", req.TaskID)
	}
}

func TestParseBoidTaskIdentityUnlink_WithProjectID(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "identity", "unlink", "jira:X-1", "--project-id=p1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ProjectID != "p1" {
		t.Fatalf("project id = %q, want p1", req.ProjectID)
	}
}

func TestParseBoidTaskIdentityUnlink_RequiresIdentity(t *testing.T) {
	cases := map[string][]string{
		"no arguments":  {"task", "identity", "unlink"},
		"too many args": {"task", "identity", "unlink", "jira:X-1", "extra"},
	}
	for name, args := range cases {
		if _, err := parseBoidRequest(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

func TestParseBoidTaskIdentityResolve(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "identity", "resolve", "jira:X-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpTaskIdentityResolve {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpTaskIdentityResolve)
	}
	if req.Identity != "jira:X-1" {
		t.Fatalf("identity = %q, want jira:X-1", req.Identity)
	}
}

func TestParseBoidTaskIdentityResolve_WithProjectID(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "identity", "resolve", "jira:X-1", "--project-id", "p1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ProjectID != "p1" {
		t.Fatalf("project id = %q, want p1", req.ProjectID)
	}
}

func TestParseBoidTaskIdentityResolve_RequiresIdentity(t *testing.T) {
	cases := map[string][]string{
		"no arguments":  {"task", "identity", "resolve"},
		"too many args": {"task", "identity", "resolve", "jira:X-1", "extra"},
	}
	for name, args := range cases {
		if _, err := parseBoidRequest(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

func TestParseBoidTaskIdentity_UnsupportedSubcommand(t *testing.T) {
	if _, err := parseBoidRequest([]string{"task", "identity", "bogus"}); err == nil {
		t.Error("expected an error for an unsupported identity subcommand")
	}
}

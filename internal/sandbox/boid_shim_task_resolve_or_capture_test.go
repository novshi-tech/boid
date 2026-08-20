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

// TestParseBoidTaskResolveOrCapture_WithStatus pins `--status` parsing
// (docs/plans/ingestion-identity.md PR-2 追記, J-9 partial retraction): the
// shim just carries the raw value through on req.Status — it does NOT
// validate it (see TestParseBoidTaskResolveOrCapture_StatusNotValidatedByShim
// below; the allowlist check lives in api.resolveLandingStatus, the single
// authoritative place per the spec).
func TestParseBoidTaskResolveOrCapture_WithStatus(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1", "--status", "triaged"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Status != "triaged" {
		t.Fatalf("status = %q, want %q", req.Status, "triaged")
	}

	req2, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1", "--status=triaged"})
	if err != nil {
		t.Fatalf("parse (=form): %v", err)
	}
	if req2.Status != "triaged" {
		t.Fatalf("status (=form) = %q, want %q", req2.Status, "triaged")
	}
}

// TestParseBoidTaskResolveOrCapture_StatusOmitted_LeavesFieldEmpty pins the
// backward-compatible default: omitting --status entirely leaves req.Status
// empty, which api.resolveLandingStatus treats as "captured" — unchanged
// pre-existing behavior for every caller that doesn't pass the new flag.
func TestParseBoidTaskResolveOrCapture_StatusOmitted_LeavesFieldEmpty(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Status != "" {
		t.Fatalf("status = %q, want empty (not given)", req.Status)
	}
}

// TestParseBoidTaskResolveOrCapture_StatusNotValidatedByShim confirms the
// shim parses ANY --status value without rejecting it — vocabulary
// validation is deliberately NOT duplicated here (spec requirement:
// 検証は api 層で必ず効くようにする — shim だけの検証は迂回されうる). A
// caller passing garbage still gets rejected, just downstream in
// TaskWorkflowService.ResolveOrCapture (internal/api/task_resolve_or_capture.go),
// not here.
func TestParseBoidTaskResolveOrCapture_StatusNotValidatedByShim(t *testing.T) {
	req, err := parseBoidRequest([]string{"task", "resolve-or-capture", "jira:X-1", "--status", "bogus"})
	if err != nil {
		t.Fatalf("parse: %v, want the shim to pass an unrecognized status through unchecked", err)
	}
	if req.Status != "bogus" {
		t.Fatalf("status = %q, want the raw unvalidated value %q", req.Status, "bogus")
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

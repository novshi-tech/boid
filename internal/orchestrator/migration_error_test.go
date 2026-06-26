package orchestrator

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestProjectMigrationError_SingleIssueByteIdentical pins the format
// produced by FormatMigrationIssue for a single issue. The golden string
// here is what rejectRemovedProjectFields used to return directly via
// fmt.Errorf("%s\n%s", combined, migrationGuidance(dir)).
func TestProjectMigrationError_SingleIssueByteIdentical(t *testing.T) {
	dir := "/home/nosen/src/github.com/novshi-tech/bm-next"
	issue := ProjectMigrationIssue{
		Dir: dir,
		Messages: []string{
			`project.yaml: top-level "kits" is no longer supported.`,
			`project.yaml: top-level "host_commands" is no longer supported.`,
		},
	}
	want := `project.yaml: top-level "kits" is no longer supported.
project.yaml: top-level "host_commands" is no longer supported.
Migration:
  1) Run: boid project migrate ` + dir + `           (dry-run)
  2) Confirm the plan, then re-run with --apply
See docs/ja/guide/migration.md for details.`

	got := FormatMigrationIssue(issue)
	if got != want {
		t.Fatalf("FormatMigrationIssue mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}

	// Error() on a 1-issue ProjectMigrationError should equal the same string.
	wrapped := &ProjectMigrationError{Projects: []ProjectMigrationIssue{issue}}
	if wrapped.Error() != want {
		t.Fatalf("ProjectMigrationError.Error() mismatch\nwant:\n%s\n\ngot:\n%s", want, wrapped.Error())
	}
}

// TestProjectMigrationError_WithProjectID checks the `project "ID": ...`
// wrapping used by project_store.LoadAll when the issue is associated with
// a registered project.
func TestProjectMigrationError_WithProjectID(t *testing.T) {
	dir := "/tmp/some/project"
	issue := ProjectMigrationIssue{
		ProjectID: "b72cd413-49b2-4456-8f42-51d28e9e2f5c",
		Dir:       dir,
		Messages: []string{
			`project.yaml: top-level "kits" is no longer supported.`,
		},
	}
	got := FormatMigrationIssue(issue)
	wantPrefix := `project "b72cd413-49b2-4456-8f42-51d28e9e2f5c": `
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("expected prefix %q, got: %s", wantPrefix, got)
	}
	if !strings.Contains(got, `top-level "kits" is no longer supported.`) {
		t.Fatalf("missing field message: %s", got)
	}
	if !strings.Contains(got, "Migration:\n  1) Run: boid project migrate "+dir) {
		t.Fatalf("missing migration guidance: %s", got)
	}
}

// TestProjectMigrationError_MultipleIssues asserts the separator between
// issues is a single newline (\n), matching what wire.go aggregates today
// at the per-line level (the "  - " prefix is added by wire.go, not by
// the type itself).
func TestProjectMigrationError_MultipleIssues(t *testing.T) {
	a := ProjectMigrationIssue{
		Dir:      "/tmp/a",
		Messages: []string{`project.yaml: top-level "kits" is no longer supported.`},
	}
	b := ProjectMigrationIssue{
		Dir:      "/tmp/b",
		Messages: []string{`project.yaml: top-level "host_commands" is no longer supported.`},
	}
	wrapped := &ProjectMigrationError{Projects: []ProjectMigrationIssue{a, b}}
	got := wrapped.Error()
	want := FormatMigrationIssue(a) + "\n" + FormatMigrationIssue(b)
	if got != want {
		t.Fatalf("multi-issue Error() mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

// TestProjectMigrationError_ErrorsAsThroughWrap verifies that errors.As
// can recover the typed error after fmt.Errorf wrapping — this is the
// critical contract for the boid start parent to pick out migration
// failures from the daemon's exit error.
func TestProjectMigrationError_ErrorsAsThroughWrap(t *testing.T) {
	inner := &ProjectMigrationError{
		Projects: []ProjectMigrationIssue{{Dir: "/x", Messages: []string{"m"}}},
	}
	wrapped := fmt.Errorf("daemon startup refused: %w", inner)
	var got *ProjectMigrationError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As failed to unwrap ProjectMigrationError")
	}
	if got != inner {
		t.Fatalf("errors.As returned different pointer")
	}
}

// TestProjectMigrationError_NilOrEmpty exercises the nil-safe path.
func TestProjectMigrationError_NilOrEmpty(t *testing.T) {
	var nilErr *ProjectMigrationError
	if nilErr.Error() == "" {
		t.Fatalf("nil Error() should produce a non-empty fallback")
	}
	empty := &ProjectMigrationError{}
	if empty.Error() == "" {
		t.Fatalf("empty Error() should produce a non-empty fallback")
	}
}

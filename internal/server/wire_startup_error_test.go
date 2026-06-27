package server

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestBuildProjectLoadStartupError_TextByteIdentical pins the legacy
// rendered text so log output (boid.log) stays unchanged when callers
// switch from the old fmt.Errorf form to the typed startupError type.
func TestBuildProjectLoadStartupError_TextByteIdentical(t *testing.T) {
	dir1 := "/home/nosen/src/github.com/novshi-tech/bm-next"
	dir2 := "/home/nosen/src/github.com/novshi-tech/boid-kits"
	mig1 := &orchestrator.ProjectMigrationError{
		Projects: []orchestrator.ProjectMigrationIssue{{
			ProjectID: "b72cd413-49b2-4456-8f42-51d28e9e2f5c",
			Dir:       dir1,
			Messages: []string{
				`project.yaml: top-level "kits" is no longer supported.`,
				`project.yaml: top-level "host_commands" is no longer supported.`,
			},
		}},
	}
	mig2 := &orchestrator.ProjectMigrationError{
		Projects: []orchestrator.ProjectMigrationIssue{{
			ProjectID: "dad1961a-9ef9-495d-858f-e27e75d9afca",
			Dir:       dir2,
			Messages:  []string{`project.yaml: top-level "kits" is no longer supported.`},
		}},
	}

	err := buildProjectLoadStartupError([]error{mig1, mig2})

	got := err.Error()
	want := "daemon startup refused: failed to load project metadata\n" +
		"  - " + mig1.Error() + "\n" +
		"  - " + mig2.Error() + "\n" +
		"Run `boid project migrate <dir>` for each affected project to migrate to the new schema.\n"
	if got != want {
		t.Fatalf("aggregate text mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

// TestBuildProjectLoadStartupError_ErrorsAs verifies that the typed
// migration error can be recovered via errors.As — this is the contract
// the boid start parent depends on for the auto-migrate path.
func TestBuildProjectLoadStartupError_ErrorsAs(t *testing.T) {
	mig := &orchestrator.ProjectMigrationError{
		Projects: []orchestrator.ProjectMigrationIssue{
			{ProjectID: "id1", Dir: "/a", Messages: []string{"m1"}},
			{ProjectID: "id2", Dir: "/b", Messages: []string{"m2"}},
		},
	}
	otherErr := errors.New("project \"xx\": something else broke")

	// Pass two migration errors so we also confirm Projects[] aggregation.
	mig2 := &orchestrator.ProjectMigrationError{
		Projects: []orchestrator.ProjectMigrationIssue{
			{ProjectID: "id3", Dir: "/c", Messages: []string{"m3"}},
		},
	}
	err := buildProjectLoadStartupError([]error{mig, otherErr, mig2})

	// Outer wrap (mirrors how runDaemonChild wraps with "create server: %w").
	wrapped := fmt.Errorf("create server: %w", err)
	var got *orchestrator.ProjectMigrationError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As failed to find ProjectMigrationError through wrap")
	}
	// All three migration issues should be aggregated, in encounter order.
	if len(got.Projects) != 3 {
		t.Fatalf("expected 3 aggregated issues, got %d (%+v)", len(got.Projects), got.Projects)
	}
	wantIDs := []string{"id1", "id2", "id3"}
	for i, p := range got.Projects {
		if p.ProjectID != wantIDs[i] {
			t.Fatalf("issue[%d] ProjectID = %q, want %q", i, p.ProjectID, wantIDs[i])
		}
	}

	// The non-migration error should still appear in the aggregate text.
	if !strings.Contains(err.Error(), `something else broke`) {
		t.Fatalf("non-migration error text missing from aggregate: %s", err.Error())
	}
}

// TestBuildProjectLoadStartupError_NoMigrationHint guards the regression
// where the migration guidance line ("Run `boid project migrate <dir>` ...")
// was always emitted, even when none of the failures were migration errors.
// That was actively misleading — it told users to run `boid project migrate`
// for things like permission errors that migrate could not fix, and made it
// look like the --auto-migrate flag should apply to all startup failures
// when in fact it only handles ProjectMigrationError.
//
// When all the errors are non-migration (e.g. parse failure), the aggregate
// text must NOT include the migration hint.
func TestBuildProjectLoadStartupError_NoMigrationHint(t *testing.T) {
	parseErr := errors.New(`project "abc": .boid/project.yaml: parse: yaml: line 3: invalid syntax`)
	err := buildProjectLoadStartupError([]error{parseErr})
	got := err.Error()
	if strings.Contains(got, "boid project migrate") {
		t.Fatalf("aggregate text must omit migration hint when no migration errors present, got:\n%s", got)
	}
	// Sanity: parse error itself should still surface.
	if !strings.Contains(got, "invalid syntax") {
		t.Fatalf("parse error text missing from aggregate: %s", got)
	}
}

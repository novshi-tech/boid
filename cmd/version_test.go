package cmd

import (
	"bytes"
	"testing"

	"github.com/novshi-tech/boid/internal/version"
	"github.com/spf13/cobra"
)

// TestVersionCmd_ScopeAndAutostartAnnotations pins the classification `boid
// version` needs cmd/scope_annotations_test.go's expectedScopeAnnotations
// table to agree with: scopeNeutral (codex review round 1 of this PR moved
// it off scopeLocal — see cmd/version.go's own annotation comment for why:
// scopeLocal is rejected outright by root's PersistentPreRunE whenever an
// https profile is selected, which is wrong for a command that reports the
// CLI binary's own identity and has nothing to do with which daemon
// --profile points at) and annotationSkipAutostart=skip (a version print
// must not spin up a daemon).
func TestVersionCmd_ScopeAndAutostartAnnotations(t *testing.T) {
	if got := versionCmd.Annotations[scopeAnnotationKey]; got != scopeNeutral {
		t.Errorf("versionCmd scope annotation = %q, want %q", got, scopeNeutral)
	}
	if got := versionCmd.Annotations[annotationSkipAutostart]; got != "skip" {
		t.Errorf("versionCmd autostart annotation = %q, want %q", got, "skip")
	}
}

// TestFormatVersionLine pins the three cases 穴2's rule distinguishes:
// an exact release tag prints bare, anything else (pseudo-version,
// "+dirty", "(devel)") is labelled "(local build)" rather than implying a
// shippable release identity, and an empty Version() (neither an ldflags
// override nor a resolvable build info) says so explicitly instead of
// printing a bare "boid ".
func TestFormatVersionLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"exact release tag", "v0.0.13", "boid v0.0.13"},
		{"pseudo-version", "v0.0.13-0.20260801120000-abcdef123456", "boid v0.0.13-0.20260801120000-abcdef123456 (local build)"},
		{"dirty suffix", "v0.0.13+dirty", "boid v0.0.13+dirty (local build)"},
		{"devel marker", "(devel)", "boid (devel) (local build)"},
		{"empty", "", "boid (version unknown)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatVersionLine(tc.in); got != tc.want {
				t.Errorf("formatVersionLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRunVersion_WritesFormattedLineToCmdOut exercises runVersion's own
// wiring (RunE writes to cmd.OutOrStdout(), one line, via
// formatVersionLine) against a throwaway *cobra.Command rather than the
// shared package-level versionCmd singleton — mutating versionCmd's
// Out/Err via SetOut/SetErr would leak across this package's other tests
// sharing the same test binary (same reasoning as root_test.go's
// newProfileTestCmd doc comment).
func TestRunVersion_WritesFormattedLineToCmdOut(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "version-test"}
	cmd.SetOut(&out)

	if err := runVersion(cmd, nil); err != nil {
		t.Fatalf("runVersion: %v", err)
	}

	want := formatVersionLine(version.Version()) + "\n"
	if out.String() != want {
		t.Errorf("runVersion output = %q, want %q", out.String(), want)
	}
}

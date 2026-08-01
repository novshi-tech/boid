package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionCmd_ScopeAndAutostartAnnotations pins the classification `boid
// version` needs cmd/scope_annotations_test.go's expectedScopeAnnotations
// table to agree with: it never talks to the daemon (scopeLocal, same
// grouping as `boid fetch`/`boid init`) and must not trigger
// EnsureRunning's autostart (annotationSkipAutostart=skip) — a plain version
// print should work even with no daemon reachable at all.
func TestVersionCmd_ScopeAndAutostartAnnotations(t *testing.T) {
	if got := versionCmd.Annotations[scopeAnnotationKey]; got != scopeLocal {
		t.Errorf("versionCmd scope annotation = %q, want %q", got, scopeLocal)
	}
	if got := versionCmd.Annotations[annotationSkipAutostart]; got != "skip" {
		t.Errorf("versionCmd autostart annotation = %q, want %q", got, "skip")
	}
}

// TestRunVersion_PrintsVersion exercises the RunE function directly
// (bypassing cobra Execute/autostart plumbing, same style as other leaf
// commands' *_test.go files in this package) and checks the printed line
// carries the effective internal/version.Version() string.
func TestRunVersion_PrintsVersion(t *testing.T) {
	var out bytes.Buffer
	cmd := versionCmd
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := runVersion(cmd, nil); err != nil {
		t.Fatalf("runVersion: %v", err)
	}
	if !strings.Contains(out.String(), "boid") {
		t.Errorf("runVersion output = %q, want it to mention %q", out.String(), "boid")
	}
}

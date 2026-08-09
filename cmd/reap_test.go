//go:build linux

package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/reap"
)

// TestReapCmd_IncludeWorkspaceHomesFlag pins the contract PR1 declares
// (docs/plans/workspace-home-volume-persistence.md 論点 a): `boid reap`
// preserves workspace HOME volumes by default and only destroys them when
// the operator explicitly asks.
func TestReapCmd_IncludeWorkspaceHomesFlag(t *testing.T) {
	f := reapCmd.Flags().Lookup("include-workspace-homes")
	if f == nil {
		t.Fatal("reap: --include-workspace-homes flag not registered")
	}
	if f.DefValue != "false" {
		t.Errorf("--include-workspace-homes default = %q, want %q (preserve by default)", f.DefValue, "false")
	}

	if got, want := reapWorkspaceHomePolicy(false), reap.PreserveWorkspaceHomes; got != want {
		t.Errorf("reapWorkspaceHomePolicy(false) = %v, want PreserveWorkspaceHomes", got)
	}
	if got, want := reapWorkspaceHomePolicy(true), reap.IncludeWorkspaceHomes; got != want {
		t.Errorf("reapWorkspaceHomePolicy(true) = %v, want IncludeWorkspaceHomes", got)
	}

	// The contract must also be discoverable from the CLI help itself, not
	// only from the plan doc.
	if !strings.Contains(reapCmd.Long, "--include-workspace-homes") {
		t.Error("reap Long description does not mention --include-workspace-homes")
	}
}

// TestPrintReapReport_SkippedVolumesPrintedBeforeNothingToReap pins the
// output ordering decision: a run that only preserved volumes still reports
// "nothing to reap" (Report.Empty()'s definition is unchanged — a skip is
// not a destroy), but the skip lines must appear FIRST so the operator sees
// why a volume they expected gone is still there.
func TestPrintReapReport_SkippedVolumesPrintedBeforeNothingToReap(t *testing.T) {
	var buf bytes.Buffer
	report := reap.Report{SkippedVolumes: []string{"boid-ws-home-abcd1234-default"}}

	if err := printReapReport(&buf, report); err != nil {
		t.Fatalf("printReapReport: %v", err)
	}

	out := buf.String()
	skipIdx := strings.Index(out, "skipped volume boid-ws-home-abcd1234-default")
	if skipIdx < 0 {
		t.Fatalf("output does not report the skipped volume:\n%s", out)
	}
	if !strings.Contains(out, "--include-workspace-homes") {
		t.Errorf("skip line does not tell the operator how to destroy it anyway:\n%s", out)
	}
	nothingIdx := strings.Index(out, "nothing to reap")
	if nothingIdx < 0 {
		t.Fatalf("skip-only report must still print %q (Report.Empty() unchanged):\n%s", "nothing to reap", out)
	}
	if skipIdx > nothingIdx {
		t.Errorf("skipped-volume line must precede %q:\n%s", "nothing to reap", out)
	}
}

// TestPrintReapReport_SkippedAlongsideDestroyed covers the non-empty case:
// skips and destructions are both reported, skips first.
func TestPrintReapReport_SkippedAlongsideDestroyed(t *testing.T) {
	var buf bytes.Buffer
	report := reap.Report{
		DestroyedVolumes: []string{"job-vol-1"},
		SkippedVolumes:   []string{"boid-ws-home-abcd1234-default"},
	}

	if err := printReapReport(&buf, report); err != nil {
		t.Fatalf("printReapReport: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "nothing to reap") {
		t.Errorf("non-empty report must not print %q:\n%s", "nothing to reap", out)
	}
	if !strings.Contains(out, "skipped volume boid-ws-home-abcd1234-default") {
		t.Errorf("output missing skip line:\n%s", out)
	}
	if !strings.Contains(out, "destroyed volume job-vol-1") {
		t.Errorf("output missing destroy line:\n%s", out)
	}
}

// TestPrintReapReport_ErrorsSurfaceAsExitError keeps the pre-existing
// "reap completed with N error(s)" behavior pinned across the refactor that
// extracted printing from runReap.
func TestPrintReapReport_ErrorsSurfaceAsExitError(t *testing.T) {
	var buf bytes.Buffer
	report := reap.Report{Errors: []string{"volume v1: boom"}}

	err := printReapReport(&buf, report)
	if err == nil {
		t.Fatal("printReapReport returned nil error for a report carrying errors")
	}
	if !strings.Contains(buf.String(), "ERROR: volume v1: boom") {
		t.Errorf("output missing the error line:\n%s", buf.String())
	}
}

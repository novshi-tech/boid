package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/testutil"
)

// resetGCFlags restores gcCmd's --older-than/--dry-run flags to their
// zero-value defaults between tests, mirroring the reset helpers other
// *_test.go files in this package use for their own package-level *cobra.Command
// singletons.
func resetGCFlags(t *testing.T) {
	t.Helper()
	if err := gcCmd.Flags().Set("older-than", "720h0m0s"); err != nil {
		t.Fatalf("reset --older-than: %v", err)
	}
	if err := gcCmd.Flags().Set("dry-run", "false"); err != nil {
		t.Fatalf("reset --dry-run: %v", err)
	}
}

// TestRunGC_NoRuntimesDirWiredIsImpossibleInPractice is intentionally absent:
// server/wire.go always wires GCHandler.RuntimesDir from runtimesDirFor(cfg),
// which is never empty for a real daemon (see runtimesDirFor's doc comment).
// The "RuntimesDir empty" branch is instead covered directly at the
// internal/api handler level (TestGCHandler_Run_NoRuntimesDir_OmitsWorkspaceHomes).

// TestRunGC_DisplaysWorkspaceHomesWithOrphanFlagAndTotal exercises the full
// CLI -> daemon -> disk round trip for `boid gc`'s workspace_homes listing
// (docs/plans/home-workspace-volume.md Phase 4 PR5): a workspace home dir
// backing a real workspace row is listed and sized, an orphan dir (no
// matching workspace row) is flagged, and the printed total sums both.
func TestRunGC_DisplaysWorkspaceHomesWithOrphanFlagAndTotal(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetGCFlags(t)
	defer resetGCFlags(t)

	testutil.SeedWorkspace(t, ts, "known-ws")
	writeWorkspaceHomeFileForTest(t, ts, "known-ws", 1000)  // -> "1.00 KB"
	writeWorkspaceHomeFileForTest(t, ts, "orphan-ws", 2000) // -> "2.00 KB", no workspace row.

	var out bytes.Buffer
	cmd := gcCmd
	cmd.SetOut(&out)
	if err := runGC(cmd, nil); err != nil {
		t.Fatalf("runGC: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "workspace homes:") {
		t.Fatalf("expected a workspace homes section, got: %s", got)
	}
	if !strings.Contains(got, "known-ws:") || strings.Contains(got, "(orphan) known-ws:") {
		t.Errorf("known-ws must be listed without an orphan prefix, got: %s", got)
	}
	if !strings.Contains(got, "(orphan) orphan-ws:") {
		t.Errorf("orphan-ws must be listed with an orphan prefix, got: %s", got)
	}
	if !strings.Contains(got, "1.00 KB") {
		t.Errorf("expected known-ws's size 1.00 KB in output, got: %s", got)
	}
	if !strings.Contains(got, "2.00 KB") {
		t.Errorf("expected orphan-ws's size 2.00 KB in output, got: %s", got)
	}
	if !strings.Contains(got, "total:") || !strings.Contains(got, "3.00 KB") {
		t.Errorf("expected a 3.00 KB total (1.00 KB + 2.00 KB), got: %s", got)
	}
}

// TestRunGC_NoWorkspaceHomesYet_OmitsSection pins that a fresh installation
// (no workspace has ever been dispatched into, so homes/ does not exist on
// disk yet) prints no "workspace homes:" section at all rather than an
// empty one.
func TestRunGC_NoWorkspaceHomesYet_OmitsSection(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetGCFlags(t)
	defer resetGCFlags(t)

	var out bytes.Buffer
	cmd := gcCmd
	cmd.SetOut(&out)
	if err := runGC(cmd, nil); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if strings.Contains(out.String(), "workspace homes:") {
		t.Errorf("expected no workspace homes section when homes/ does not exist, got: %s", out.String())
	}
}

// TestRunGC_DryRun_StillReportsWorkspaceHomes pins that --dry-run does not
// suppress the size listing — it is visibility-only reporting, not a
// pending mutation, so there is nothing for --dry-run to gate.
func TestRunGC_DryRun_StillReportsWorkspaceHomes(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetGCFlags(t)
	defer resetGCFlags(t)

	testutil.SeedWorkspace(t, ts, "known-ws")
	writeWorkspaceHomeFileForTest(t, ts, "known-ws", 500)
	if err := gcCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set --dry-run: %v", err)
	}

	var out bytes.Buffer
	cmd := gcCmd
	cmd.SetOut(&out)
	if err := runGC(cmd, nil); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "dry run:") {
		t.Errorf("expected the dry-run delete-stats line, got: %s", got)
	}
	if !strings.Contains(got, "workspace homes:") {
		t.Errorf("expected workspace homes listing even under --dry-run, got: %s", got)
	}
}

// TestPrintWorkspaceHomes_UnitFormatting is a focused unit test of the
// rendering helper itself, independent of any daemon/HTTP round trip: pins
// the "(orphan) " prefix, the "?" fallback for a size error (excluded from
// the total), and the trailing total line.
func TestPrintWorkspaceHomes_UnitFormatting(t *testing.T) {
	var out bytes.Buffer
	printWorkspaceHomes(&out, []api.WorkspaceHomeSize{
		{Slug: "default", Bytes: 1000},
		{Slug: "orphan-ws", Bytes: 2000, Orphan: true},
		{Slug: "broken-ws", SizeError: "permission denied"},
	}, "")

	// tabwriter pads columns with spaces (not literal tabs) once flushed, so
	// assert against each line with its internal whitespace collapsed back
	// down to single spaces, rather than a literal "\t"-joined substring.
	got := out.String()
	normalized := normalizeWhitespaceLines(got)
	for _, want := range []string{
		"default: 1.00 KB",
		"(orphan) orphan-ws: 2.00 KB",
		"broken-ws: ?",
		// total excludes broken-ws's unknown size: 1000 + 2000 = 3000 -> 3.00 KB.
		"total: 3.00 KB",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("expected a line matching %q; normalized output: %q\nraw output: %s", want, normalized, got)
		}
	}
}

// normalizeWhitespaceLines collapses each line's internal run of whitespace
// (tabwriter's column padding) down to single spaces and trims its ends, so
// tests can assert against a stable "label: value" substring regardless of
// how many padding spaces tabwriter chose for column alignment.
func normalizeWhitespaceLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	return strings.Join(lines, "\n")
}

func TestPrintWorkspaceHomes_EmptyList_NoOutput(t *testing.T) {
	var out bytes.Buffer
	printWorkspaceHomes(&out, nil, "")
	if out.Len() != 0 {
		t.Errorf("expected no output for an empty list, got: %s", out.String())
	}
}

// TestPrintWorkspaceHomes_ListError_PrintsWarningNotEmptyTable pins
// Should-fix #3 (codex PR #791 review): a non-empty listErr must render a
// single warning line, distinct from silently printing nothing (which would
// be indistinguishable from "no workspace has ever been dispatched into
// yet") and distinct from the old "every entry mismarked orphan" behavior.
func TestPrintWorkspaceHomes_ListError_PrintsWarningNotEmptyTable(t *testing.T) {
	var out bytes.Buffer
	printWorkspaceHomes(&out, nil, "db unavailable")
	got := out.String()
	if !strings.Contains(got, "db unavailable") {
		t.Errorf("expected the list error message in output, got: %q", got)
	}
	if strings.Contains(got, "total:") {
		t.Errorf("expected no total line when the listing itself was omitted, got: %q", got)
	}
}

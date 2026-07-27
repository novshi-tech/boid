package cmd

import (
	"bytes"
	"os"
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

// TestRunGC_NoHomeStoreWiredIsImpossibleInPractice is intentionally absent:
// server/wire.go always wires GCHandler.Homes for a real daemon (it is nil
// only when cfg.Backend injected a test/DI sandbox backend, so no docker
// client was built at all). The nil branch is covered directly at the
// internal/api handler level (TestGCHandler_Run_NoHomeStore_OmitsWorkspaceHomes).

// The workspace_homes listing became engine-backed in PR7 of
// docs/plans/workspace-home-volume-persistence.md (論点 a-2), so the "seed a
// directory, then assert `boid gc` sizes it" round trip these tests used to
// run is no longer expressible without a live docker/podman engine — the
// daemon enumerates VOLUMES now, and no amount of writing files under
// runtimes/ produces one. That coverage moved to where it can be exercised
// deterministically:
//
//   - the orphan flag, the sizes and the lister-failure degrade:
//     internal/api's TestGCHandler_Run_WithHomeStore_* cases;
//   - the engine calls behind them: internal/dispatcher's
//     TestWorkspaceHomeVolumeStore_* cases;
//   - the rendering itself: TestPrintWorkspaceHomes_UnitFormatting below,
//     which was already a pure unit test of the helper.
//
// What stays here is the part that IS observable end to end without an
// engine, and that matters more than the listing does: `boid gc` still does
// its actual job when the volume listing cannot be produced, and it says so.
//
// TestRunGC_NoWorkspaceHomesYet_OmitsSection is also gone, and not for the
// same reason. Its premise — a daemon with a reachable engine that has no
// workspace home volumes on it — is not something this suite can construct at
// all (homeenv points DOCKER_HOST at a socket nothing answers, deliberately:
// see testutil.NewTestServer's guard). What it actually observed was an
// enumeration failure rendering as silence, which is the very defect PR7
// round-2 review Major 2 fixed. The property it meant to pin — an empty
// listing prints no section — is unchanged and covered deterministically by
// TestPrintWorkspaceHomes_EmptyList_NoOutput below.

// TestRunGC_EngineUnavailable_ReportsWhyAndStillDoesItsOwnWork pins both
// halves of `boid gc`'s behavior when the engine cannot enumerate volumes.
//
// Non-fatal: the record deletion has already happened by then, and losing its
// report over a docker socket that is not there would be a much worse trade
// than losing the size listing.
//
// Not silent (PR7 round-2 codex review, Major 2): the reason has to reach the
// operator. Printing only the delete-stats line makes a wedged engine look
// exactly like an install that has never been dispatched into — and the size
// listing is the only place `boid gc` would ever have mentioned the engine at
// all, so there is no other line for the operator to notice something is
// wrong in.
func TestRunGC_EngineUnavailable_ReportsWhyAndStillDoesItsOwnWork(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetGCFlags(t)
	defer resetGCFlags(t)

	testutil.SeedWorkspace(t, ts, "known-ws")

	var out bytes.Buffer
	cmd := gcCmd
	cmd.SetOut(&out)
	if err := runGC(cmd, nil); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "deleted:") {
		t.Errorf("expected the delete-stats line, got: %s", got)
	}
	if !strings.Contains(got, "listing unavailable") {
		t.Errorf("expected the workspace-home listing failure to be reported, got: %s", got)
	}
	// The isolated engine socket is what the daemon failed to reach, so its
	// path is what the reason has to name. Asserting on the reason's CONTENT,
	// not merely its presence, is what keeps this from passing on a generic
	// "unavailable" with the cause dropped.
	if !strings.Contains(got, os.Getenv("DOCKER_HOST")[len("unix://"):]) {
		t.Errorf("expected the reason to name the engine that could not be reached, got: %s", got)
	}
	if strings.Contains(got, "total:") {
		t.Errorf("expected no size table when the listing itself could not be produced, got: %s", got)
	}
}

// TestRunGC_DryRun_StillCompletes keeps the --dry-run path exercised end to
// end. Its former companion assertion — that --dry-run does not SUPPRESS the
// listing, since visibility-only reporting has no pending mutation for
// --dry-run to gate — moved to internal/api's
// TestGCHandler_Run_DryRun_StillListsWorkspaceHomes, which can wire a home
// store and therefore actually observe the listing.
func TestRunGC_DryRun_StillCompletes(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetGCFlags(t)
	defer resetGCFlags(t)

	testutil.SeedWorkspace(t, ts, "known-ws")
	if err := gcCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set --dry-run: %v", err)
	}

	var out bytes.Buffer
	cmd := gcCmd
	cmd.SetOut(&out)
	if err := runGC(cmd, nil); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "dry run:") {
		t.Errorf("expected the dry-run delete-stats line, got: %s", got)
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

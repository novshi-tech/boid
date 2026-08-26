package cmd

// docs/plans/signal-ingest-detailed-design.md §3.1 (PR-2): CLI-level tests
// for `boid signal list` / `boid signal ack`, against a real daemon
// (testutil.NewTestServer — real sqlite DB, the actual SignalHandler wired
// through internal/server/wire.go's mountRoutes) rather than a stub HTTP
// server, so these also prove the route is actually mounted and reachable
// end to end, not just that internal/api's handler behaves correctly in
// isolation (that's internal/api/signal_handler_test.go's job).
//
// signal-driven-review.md §14 Q14 ("`boid signal ack` は冪等で、二重 ack が
// エラーにならないテストがある") is pinned here (TestRunSignalAck_Idempotent)
// as well as at the handler layer.
//
// Seeding uses orchestrator.IngestSignals directly against ts.Server.DB() —
// PR-2 does not add a host-level ingest endpoint (that is PR-3's
// connector-only shim op, out of scope here per the parallel-worktree
// split), so tests reach into the same DB the daemon under test already
// owns rather than going through the CLI/HTTP surface for setup.

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

// newSignalCmdForTest points cmd (signalListCmd or signalAckCmd, both
// package-level singletons) at ts's daemon and captures its output. Flags
// are reset and re-registered so state from an earlier test in this package
// can't leak in (the same discipline newTaskCreateCmd/newInitScriptCmd use
// elsewhere in this package).
func newSignalCmdForTest(t *testing.T, cmd *cobra.Command, register func(*cobra.Command), ts *testutil.TestServer) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd.ResetFlags()
	register(cmd)

	prevCtx := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prevCtx)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})

	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), ts.Client))
	return cmd, out
}

// withOutputFormat temporarily overrides rootCmd's persistent -o/--output
// flag (renderOutput reads it via cmd.Root()), restoring "plain" (its
// registered default) on cleanup.
func withOutputFormat(t *testing.T, format string) {
	t.Helper()
	if err := rootCmd.PersistentFlags().Set("output", format); err != nil {
		t.Fatalf("set --output=%s: %v", format, err)
	}
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("output", "plain")
	})
}

func seedCLISignal(t *testing.T, ts *testutil.TestServer, ws, service, connector string, row orchestrator.SignalIngestRow) {
	t.Helper()
	repo := orchestrator.NewTaskRepository(ts.Server.DB())
	if err := repo.IngestSignals(ws, service, connector, []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("seed signal %q: %v", row.ID, err)
	}
}

// runSignalListIDs runs `boid signal list` (via runSignalList against cmd,
// whatever flags the caller already set on it) with -o json and returns the
// sorted set of returned signal ids — a precise way to assert exactly which
// rows a filter did/didn't select, rather than substring-matching plain
// output.
func runSignalListIDs(t *testing.T, cmd *cobra.Command) []string {
	t.Helper()
	withOutputFormat(t, "json")
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	if err := runSignalList(cmd, nil); err != nil {
		t.Fatalf("runSignalList: %v", err)
	}
	var signals []apiwire.Signal
	if err := json.Unmarshal(out.Bytes(), &signals); err != nil {
		t.Fatalf("decode json output: %v, output = %s", err, out.String())
	}
	ids := make([]string, len(signals))
	for i, s := range signals {
		ids[i] = s.ID
	}
	sort.Strings(ids)
	return ids
}

// TestRunSignalList_DefaultWorkspace pins that omitting --workspace scopes
// the list to the "default" slug (signal-ingest-detailed-design.md §3.1),
// excluding a signal seeded into a different workspace.
func TestRunSignalList_DefaultWorkspace(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T02:23:48Z", Identity: "slack-thread:1", Title: "hello world",
	})
	seedCLISignal(t, ts, "other-ws", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:2", OccurredAt: "2026-08-26T02:24:00Z", Identity: "slack-thread:2", Title: "should not appear",
	})

	cmd, out := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
	if err := runSignalList(cmd, nil); err != nil {
		t.Fatalf("runSignalList: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "slack:1") {
		t.Errorf("output = %q, want it to contain the default-workspace signal slack:1", got)
	}
	if strings.Contains(got, "slack:2") {
		t.Errorf("output = %q, must NOT contain other-ws's signal slack:2", got)
	}
}

// TestRunSignalList_SourceFilter pins that --source <pack>/<connector> is
// forwarded to the server unsplit, matching the stored composite value.
func TestRunSignalList_SourceFilter(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})
	seedCLISignal(t, ts, "default", "jira-api", "jira-cloud/jira-cloud", orchestrator.SignalIngestRow{
		ID: "jira:1", OccurredAt: "2026-08-26T02:00:00Z", Identity: "jira:PROJ-1",
	})

	cmd, out := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
	if err := cmd.Flags().Set("source", "slack/mentions"); err != nil {
		t.Fatalf("set --source: %v", err)
	}
	if err := runSignalList(cmd, nil); err != nil {
		t.Fatalf("runSignalList: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "slack:1") {
		t.Errorf("output = %q, want the slack signal", got)
	}
	if strings.Contains(got, "jira:1") {
		t.Errorf("output = %q, must NOT contain the jira signal (source filter)", got)
	}
}

// TestRunSignalList_WorkspaceFlag_NonDefault pins F2 (Opus review, PR
// #1011): passing --workspace <slug> must actually reach that workspace's
// inbox, not just "not the default" (TestRunSignalList_DefaultWorkspace
// only proves the OMITTED-flag path; a mutant that hardcodes the
// workspace_id query value to "default" regardless of the --workspace flag
// would still pass that test). The signal here is seeded ONLY in "team-a",
// so it is invisible unless --workspace team-a genuinely reaches the
// server.
func TestRunSignalList_WorkspaceFlag_NonDefault(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "team-a", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:team-a", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	cmd, _ := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
	if err := cmd.Flags().Set("workspace", "team-a"); err != nil {
		t.Fatalf("set --workspace: %v", err)
	}
	ids := runSignalListIDs(t, cmd)
	if len(ids) != 1 || ids[0] != "slack:team-a" {
		t.Fatalf("ids = %v, want exactly [slack:team-a] with --workspace team-a", ids)
	}
}

// TestRunSignalList_StateFlag pins F2's --state coverage: seeds one pending
// and one acked signal in the same workspace, then asserts --state
// pending/acked/all each select the exact expected set. A mutant that
// mangles the "state=" query key (e.g. Opus review's example of renaming it
// to something the server ignores) would make every one of these calls fall
// through to the server's pending default, and this test would catch that
// via the acked/all cases.
func TestRunSignalList_StateFlag(t *testing.T) {
	ts := testutil.NewTestServer(t)
	ws := "ws-state"
	seedCLISignal(t, ts, ws, "svc", "pack/conn", orchestrator.SignalIngestRow{
		ID: "pend:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "id:pend",
	})
	seedCLISignal(t, ts, ws, "svc", "pack/conn", orchestrator.SignalIngestRow{
		ID: "acked:1", OccurredAt: "2026-08-26T02:00:00Z", Identity: "id:acked",
	})
	repo := orchestrator.NewTaskRepository(ts.Server.DB())
	if err := repo.AckSignals(ws, []string{"acked:1"}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	newCmd := func(t *testing.T, state string) *cobra.Command {
		cmd, _ := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
		if err := cmd.Flags().Set("workspace", ws); err != nil {
			t.Fatalf("set --workspace: %v", err)
		}
		if state != "" {
			if err := cmd.Flags().Set("state", state); err != nil {
				t.Fatalf("set --state: %v", err)
			}
		}
		return cmd
	}

	t.Run("pending", func(t *testing.T) {
		ids := runSignalListIDs(t, newCmd(t, "pending"))
		if len(ids) != 1 || ids[0] != "pend:1" {
			t.Fatalf("ids = %v, want exactly [pend:1]", ids)
		}
	})
	t.Run("acked", func(t *testing.T) {
		ids := runSignalListIDs(t, newCmd(t, "acked"))
		if len(ids) != 1 || ids[0] != "acked:1" {
			t.Fatalf("ids = %v, want exactly [acked:1]", ids)
		}
	})
	t.Run("all", func(t *testing.T) {
		ids := runSignalListIDs(t, newCmd(t, "all"))
		if len(ids) != 2 || ids[0] != "acked:1" || ids[1] != "pend:1" {
			t.Fatalf("ids = %v, want exactly [acked:1 pend:1]", ids)
		}
	})
}

// TestRunSignalList_ServiceFilter pins F2's --service coverage, the same
// way TestRunSignalList_SourceFilter does for --source.
func TestRunSignalList_ServiceFilter(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})
	seedCLISignal(t, ts, "default", "jira-api", "jira-cloud/jira-cloud", orchestrator.SignalIngestRow{
		ID: "jira:1", OccurredAt: "2026-08-26T02:00:00Z", Identity: "jira:PROJ-1",
	})

	cmd, _ := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
	if err := cmd.Flags().Set("service", "jira-api"); err != nil {
		t.Fatalf("set --service: %v", err)
	}
	ids := runSignalListIDs(t, cmd)
	if len(ids) != 1 || ids[0] != "jira:1" {
		t.Fatalf("ids = %v, want exactly [jira:1] with --service jira-api", ids)
	}
}

// TestRunSignalList_LimitFlag pins F2's --limit coverage.
func TestRunSignalList_LimitFlag(t *testing.T) {
	ts := testutil.NewTestServer(t)
	occurredAt := []string{"2026-08-26T01:00:00Z", "2026-08-26T02:00:00Z", "2026-08-26T03:00:00Z"}
	for i, id := range []string{"slack:1", "slack:2", "slack:3"} {
		seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
			ID: id, OccurredAt: occurredAt[i], Identity: "id:" + id,
		})
	}

	cmd, _ := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
	if err := cmd.Flags().Set("limit", "1"); err != nil {
		t.Fatalf("set --limit: %v", err)
	}
	ids := runSignalListIDs(t, cmd)
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want exactly 1 id with --limit 1", ids)
	}
}

// TestRunSignalList_JSONOutput pins the envelope's source.pack/
// source.connector split reaches -o json (signal-driven-review.md §5.2).
func TestRunSignalList_JSONOutput(t *testing.T) {
	withOutputFormat(t, "json")
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	cmd, out := newSignalCmdForTest(t, signalListCmd, registerSignalListFlags, ts)
	if err := runSignalList(cmd, nil); err != nil {
		t.Fatalf("runSignalList: %v", err)
	}

	got := out.String()
	for _, want := range []string{`"pack": "slack"`, `"connector": "mentions"`, `"service": "slack-api"`} {
		if !strings.Contains(got, want) {
			t.Errorf("json output = %s, want it to contain %s", got, want)
		}
	}
}

// TestRunSignalAck_DefaultWorkspace pins the ack side's --workspace default.
func TestRunSignalAck_DefaultWorkspace(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	cmd, _ := newSignalCmdForTest(t, signalAckCmd, registerSignalAckFlags, ts)
	if err := runSignalAck(cmd, []string{"slack:1"}); err != nil {
		t.Fatalf("runSignalAck: %v", err)
	}

	repo := orchestrator.NewTaskRepository(ts.Server.DB())
	acked, err := repo.ListSignals(orchestrator.SignalFilter{WorkspaceID: "default", State: orchestrator.SignalStateAcked})
	if err != nil {
		t.Fatalf("list acked: %v", err)
	}
	if len(acked) != 1 || acked[0].ID != "slack:1" {
		t.Fatalf("acked signals in default workspace = %+v, want [slack:1]", acked)
	}
}

// TestRunSignalAck_WorkspaceFlag_NonDefault pins F2 (Opus review, PR #1011):
// --workspace <slug> on `boid signal ack` must reach that workspace, not
// silently fall back to "default". The signal here exists ONLY in "team-a",
// so a mutant that hardcodes AckSignalsRequest.WorkspaceID to "" (which the
// server then defaults to "default") would make this ack fail with an
// "unknown id" error, since slack:team-a doesn't exist in "default" —
// exactly the failure mode this test guards against.
func TestRunSignalAck_WorkspaceFlag_NonDefault(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "team-a", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:team-a", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	cmd, _ := newSignalCmdForTest(t, signalAckCmd, registerSignalAckFlags, ts)
	if err := cmd.Flags().Set("workspace", "team-a"); err != nil {
		t.Fatalf("set --workspace: %v", err)
	}
	if err := runSignalAck(cmd, []string{"slack:team-a"}); err != nil {
		t.Fatalf("runSignalAck: %v", err)
	}

	repo := orchestrator.NewTaskRepository(ts.Server.DB())
	acked, err := repo.ListSignals(orchestrator.SignalFilter{WorkspaceID: "team-a", State: orchestrator.SignalStateAcked})
	if err != nil {
		t.Fatalf("list acked: %v", err)
	}
	if len(acked) != 1 || acked[0].ID != "slack:team-a" {
		t.Fatalf("acked signals in team-a = %+v, want [slack:team-a]", acked)
	}
	// And it must NOT have landed in "default" instead.
	defaultAcked, err := repo.ListSignals(orchestrator.SignalFilter{WorkspaceID: "default", State: orchestrator.SignalStateAcked})
	if err != nil {
		t.Fatalf("list acked in default: %v", err)
	}
	if len(defaultAcked) != 0 {
		t.Fatalf("acked signals in default = %+v, want none (the signal belongs to team-a)", defaultAcked)
	}
}

// TestRunSignalAck_Idempotent pins Q14 at the CLI layer: running `boid
// signal ack <id>` twice must succeed both times.
func TestRunSignalAck_Idempotent(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedCLISignal(t, ts, "default", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	for i, label := range []string{"first ack", "second ack (idempotent)"} {
		cmd, out := newSignalCmdForTest(t, signalAckCmd, registerSignalAckFlags, ts)
		if err := runSignalAck(cmd, []string{"slack:1"}); err != nil {
			t.Fatalf("%s (call %d): runSignalAck: %v", label, i, err)
		}
		if !strings.Contains(out.String(), "1") {
			t.Errorf("%s: output = %q, want it to report 1 acked", label, out.String())
		}
	}
}

// TestRunSignalAck_UnknownID_Errors pins that acking a nonexistent id
// surfaces as a CLI error (typo detection, signal-ingest-detailed-design.md
// §2), not a silent success.
func TestRunSignalAck_UnknownID_Errors(t *testing.T) {
	ts := testutil.NewTestServer(t)

	cmd, _ := newSignalCmdForTest(t, signalAckCmd, registerSignalAckFlags, ts)
	err := runSignalAck(cmd, []string{"no-such-id"})
	if err == nil {
		t.Fatal("runSignalAck returned nil for an unknown id, want an error")
	}
}

// TestSignalCommands_ScopeAnnotations pins that both leaf commands declare
// boid.scope — the general sweep in scope_annotations_test.go already
// enforces this build-wide, this just documents the expectation locally
// next to the commands it's about.
func TestSignalCommands_ScopeAnnotations(t *testing.T) {
	for _, cmd := range []*cobra.Command{signalListCmd, signalAckCmd} {
		if got := cmd.Annotations[scopeAnnotationKey]; got != scopeRemote {
			t.Errorf("%s: boid.scope = %q, want %q", cmd.CommandPath(), got, scopeRemote)
		}
	}
}

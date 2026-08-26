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
	"strings"
	"testing"

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

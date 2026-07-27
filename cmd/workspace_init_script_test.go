package cmd

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d), CLI half:
// `boid workspace get-init-script / set-init-script / edit-init-script /
// unset-init-script`.
//
// Every test here runs against a REAL daemon (testutil.NewTestServer) rather
// than a stubbed client, because the property the whole PR exists for is a
// wiring one: the bytes an operator hands the CLI have to end up at the path
// the dispatcher reads. A stubbed HTTP layer would confirm the CLI sends
// something and prove nothing about where it lands.
//
// NewTestServer points $XDG_CONFIG_HOME at a throwaway directory, and the CLI
// and daemon share this process, so the daemon-side path is observable from
// here — see daemonInitScriptPath.

// newInitScriptCmd binds a workspace subcommand to the test server and
// captures its output.
//
// The command objects are package-level singletons, so the context is RESTORED
// on cleanup. Without that, a later test in this package that relies on
// client.FromContext's env-based fallback (the pattern cmd/workspace_test.go's
// export tests use) would instead find this test's client — pointing at a
// server that has since been torn down — and fail with a connection error that
// names a temp directory belonging to a test that already finished.
func newInitScriptCmd(t *testing.T, c *cobra.Command, ts *testutil.TestServer, out *bytes.Buffer) *cobra.Command {
	t.Helper()
	prev := c.Context()
	t.Cleanup(func() { c.SetContext(prev) })
	c.SetOut(out)
	c.SetErr(out)
	c.SetContext(client.WithClient(context.Background(), ts.Client))
	return c
}

// seedInitScriptWorkspace creates a workspace row so the init-script endpoints
// have something to hang off (they 404 on an unknown slug by design).
func seedInitScriptWorkspace(t *testing.T, ts *testutil.TestServer, slug string) {
	t.Helper()
	body := []byte("slug: " + slug + "\n")
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", body, nil); err != nil {
		t.Fatalf("seed workspace %q: %v", slug, err)
	}
}

// daemonInitScriptPath is where the DAEMON keeps slug's init.sh, derived from
// the environment NewTestServer isolated — deliberately not from the CLI's own
// output, so an assertion about "it landed in the right place" is independent
// of the code under test.
func daemonInitScriptPath(t *testing.T, slug string) string {
	t.Helper()
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		t.Fatal("XDG_CONFIG_HOME is unset; NewTestServer should have isolated it")
	}
	return filepath.Join(configDir, "boid", "workspaces", slug, "init.sh")
}

func writeTempScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "init.sh")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp script: %v", err)
	}
	return path
}

func resetInitScriptFlags(t *testing.T) {
	t.Helper()
	workspaceInitScriptFile = ""
	workspaceInitScriptOutput = ""
	workspaceInitScriptForce = false
}

// TestWorkspaceSetInitScript_LandsWhereTheDaemonReadsIt is the CLI-to-disk
// seam: `set-init-script` on the client side, the daemon's own config root on
// the other.
func TestWorkspaceSetInitScript_LandsWhereTheDaemonReadsIt(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	script := "#!/bin/sh\ncurl -fsSL https://example.invalid/install.sh | bash\n"
	workspaceInitScriptFile = writeTempScript(t, script)

	var out bytes.Buffer
	cmd := newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &out)
	if err := runWorkspaceSetInitScript(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceSetInitScript: %v", err)
	}

	got, err := os.ReadFile(daemonInitScriptPath(t, "team-a"))
	if err != nil {
		t.Fatalf("the script never reached the daemon's config root: %v", err)
	}
	if string(got) != script {
		t.Errorf("daemon-side init.sh = %q, want %q", got, script)
	}
	if !strings.Contains(out.String(), "written") {
		t.Errorf("stdout = %q, want it to report the write", out.String())
	}
	// The reported location is the DAEMON's, and the CLI has to say so — an
	// operator reading a bare path assumes it is on their own machine, which
	// is the exact confusion this PR is fixing in the adapters' messages.
	if !strings.Contains(out.String(), daemonInitScriptPath(t, "team-a")) {
		t.Errorf("stdout = %q, want it to name the daemon-side path", out.String())
	}
}

// TestWorkspaceGetInitScript_RoundTripsTheExactBytes pins fidelity through the
// full CLI round trip. The bytes are hashed into the completion marker, so a
// newline gained or lost is a spurious re-init of the whole workspace.
func TestWorkspaceGetInitScript_RoundTripsTheExactBytes(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	script := "echo one\n\techo tabbed\necho no-trailing-newline"
	workspaceInitScriptFile = writeTempScript(t, script)
	var setOut bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &setOut), []string{"team-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	var out bytes.Buffer
	if err := runWorkspaceGetInitScript(newInitScriptCmd(t, workspaceGetInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.String() != script {
		t.Errorf("get-init-script stdout = %q, want the exact bytes %q", out.String(), script)
	}
}

// TestWorkspaceGetInitScript_WritesToOutputFile covers -o, the form an
// operator uses to pull a script down, edit it, and push it back.
func TestWorkspaceGetInitScript_WritesToOutputFile(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	workspaceInitScriptFile = writeTempScript(t, "echo hi\n")
	var setOut bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &setOut), []string{"team-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "pulled.sh")
	workspaceInitScriptOutput = dest
	var out bytes.Buffer
	if err := runWorkspaceGetInitScript(newInitScriptCmd(t, workspaceGetInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("get -o: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read -o file: %v", err)
	}
	if string(data) != "echo hi\n" {
		t.Errorf("-o file = %q, want %q", data, "echo hi\n")
	}
}

// TestWorkspaceGetInitScript_AbsentIsAnError: a workspace with no init.sh
// exits non-zero rather than printing nothing successfully.
//
// An empty script is not a state boid keeps — uploading one clears the script
// (internal/api's workspaceInitScriptContentIsAbsent) — so empty output could
// only ever mean "there is no script". A zero exit alongside it would leave a
// pipeline unable to tell that from a script that happens to produce no
// output when read, and would make `get | set` on another workspace a silent
// clear.
func TestWorkspaceGetInitScript_AbsentIsAnError(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	var out bytes.Buffer
	err := runWorkspaceGetInitScript(newInitScriptCmd(t, workspaceGetInitScriptCmd, ts, &out), []string{"team-a"})
	if err == nil {
		t.Fatal("get-init-script on a workspace with no init.sh succeeded")
	}
	if !strings.Contains(err.Error(), "no init.sh") {
		t.Errorf("error = %q, want it to say the workspace has no init.sh", err)
	}
}

// TestWorkspaceSetInitScript_RejectsNULBeforeAnyDaemonCall pins the
// client-side fail-fast half of D3 — the same double-validation `workspace
// edit` and `workspace import` already do. The daemon rejects it too; failing
// here saves a round trip and works with no daemon reachable at all.
func TestWorkspaceSetInitScript_RejectsNULBeforeAnyDaemonCall(t *testing.T) {
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	workspaceInitScriptFile = writeTempScript(t, "echo hi\x00\n")

	var out bytes.Buffer
	cmd := workspaceSetInitScriptCmd
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// Deliberately NO client in the context: reaching the daemon at all would
	// panic, which is how this test proves the check runs first.
	cmd.SetContext(context.Background())
	err := runWorkspaceSetInitScript(cmd, []string{"team-a"})
	if err == nil {
		t.Fatal("set-init-script accepted a script containing a NUL byte")
	}
	if !strings.Contains(err.Error(), "NUL") {
		t.Errorf("error = %q, want it to name the NUL byte", err)
	}
}

// TestWorkspaceUnsetInitScript_RemovesTheFile covers the clear path end to
// end.
func TestWorkspaceUnsetInitScript_RemovesTheFile(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	workspaceInitScriptFile = writeTempScript(t, "echo hi\n")
	var setOut bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &setOut), []string{"team-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	var out bytes.Buffer
	if err := runWorkspaceUnsetInitScript(newInitScriptCmd(t, workspaceUnsetInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if _, err := os.Stat(daemonInitScriptPath(t, "team-a")); !os.IsNotExist(err) {
		t.Errorf("init.sh survived unset-init-script (stat err = %v)", err)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Errorf("stdout = %q, want it to report the clear", out.String())
	}
}

// newRacingInitScriptClient returns a client that reaches the test daemon
// through one extra HTTP hop, with beforePut run just before any PUT is
// forwarded.
//
// That hop is the seam this package otherwise has no way to reach.
// `set-init-script` fetches the current revision and immediately writes it back
// as If-Match, so from outside there is no moment at which a concurrent change
// can be made to land in between — and a goroutine trying would be testing the
// scheduler. Putting the concurrent write in front of the forwarded PUT makes
// the interleaving exact: the CLI's GET has already returned, and the daemon
// has not yet seen its write.
//
// The proxy is a plain reverse proxy onto the daemon's own UNIX socket, and
// the CLI drives it through the ordinary loopback-http client constructor, so
// every layer under test — revision fetch, If-Match header, status handling —
// is the production one.
func newRacingInitScriptClient(t *testing.T, ts *testutil.TestServer, beforePut func()) *client.Client {
	t.Helper()
	socket := ts.Client.SocketPath()
	if socket == "" {
		t.Fatal("the test server's client is not a unix client; there is no socket to proxy onto")
	}
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "boid"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && beforePut != nil {
			beforePut()
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build a loopback client for %s: %v", srv.URL, err)
	}
	return c
}

// TestWorkspaceSetInitScript_StaleRevisionIsReportedNotOverwritten pins the
// If-Match guard THROUGH THE CLI (PR9 codex round 2, Minor 3).
//
// The conflicting write used to be issued by the test itself, straight at the
// client's PutRawWithIfMatch, which meant the assertion was about the daemon
// and not about `boid workspace set-init-script` at all: the CLI could stop
// sending the revision it just read, or stop turning a 412 into an error, and
// this test would have gone on passing. Now the command is the only thing that
// writes, and the conflict is landed underneath it — so both of those
// regressions fail here.
func TestWorkspaceSetInitScript_StaleRevisionIsReportedNotOverwritten(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	workspaceInitScriptFile = writeTempScript(t, "v1\n")
	var out bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("set v1: %v", err)
	}

	// A second operator (or the Web UI) lands a change in the window between
	// this command's read and its write.
	landed := false
	racing := newRacingInitScriptClient(t, ts, func() {
		if landed {
			return
		}
		landed = true
		if _, _, err := ts.Client.PutRawWithIfMatch("/api/workspaces/team-a/init-script?force=true",
			"text/x-shellscript", []byte("landed-elsewhere\n"), ""); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	})

	workspaceInitScriptFile = writeTempScript(t, "v2\n")
	cmd := workspaceSetInitScriptCmd
	prev := cmd.Context()
	t.Cleanup(func() { cmd.SetContext(prev) })
	var raceOut bytes.Buffer
	cmd.SetOut(&raceOut)
	cmd.SetErr(&raceOut)
	cmd.SetContext(client.WithClient(context.Background(), racing))

	err := runWorkspaceSetInitScript(cmd, []string{"team-a"})
	if err == nil {
		t.Fatalf("set-init-script overwrote a script that changed under it; stdout = %q", raceOut.String())
	}
	if !landed {
		t.Fatal("the concurrent write never ran; the test proved nothing")
	}
	// The operator has to be told to retry rather than shown a raw status.
	if !strings.Contains(err.Error(), "re-run") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %q, want it to name the retry and --force", err)
	}
	got, err := os.ReadFile(daemonInitScriptPath(t, "team-a"))
	if err != nil {
		t.Fatalf("read daemon init.sh: %v", err)
	}
	if string(got) != "landed-elsewhere\n" {
		t.Errorf("daemon init.sh = %q, want the concurrent write to be intact", got)
	}
}

// TestWorkspaceUnsetInitScript_StaleRevisionIsReportedNotOverwritten is the
// same guard on the delete verb, where getting it wrong destroys rather than
// replaces.
func TestWorkspaceUnsetInitScript_StaleRevisionIsReportedNotOverwritten(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	workspaceInitScriptFile = writeTempScript(t, "v1\n")
	var out bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("set v1: %v", err)
	}

	landed := false
	racing := newRacingInitScriptClient(t, ts, func() {
		if landed {
			return
		}
		landed = true
		if _, _, err := ts.Client.PutRawWithIfMatch("/api/workspaces/team-a/init-script?force=true",
			"text/x-shellscript", []byte("landed-elsewhere\n"), ""); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	})

	cmd := workspaceUnsetInitScriptCmd
	prev := cmd.Context()
	t.Cleanup(func() { cmd.SetContext(prev) })
	var raceOut bytes.Buffer
	cmd.SetOut(&raceOut)
	cmd.SetErr(&raceOut)
	cmd.SetContext(client.WithClient(context.Background(), racing))

	if err := runWorkspaceUnsetInitScript(cmd, []string{"team-a"}); err == nil {
		t.Fatalf("unset-init-script deleted a script that changed under it; stdout = %q", raceOut.String())
	}
	if !landed {
		t.Fatal("the concurrent write never ran; the test proved nothing")
	}
	got, err := os.ReadFile(daemonInitScriptPath(t, "team-a"))
	if err != nil {
		t.Fatalf("the concurrent write was deleted: %v", err)
	}
	if string(got) != "landed-elsewhere\n" {
		t.Errorf("daemon init.sh = %q, want the concurrent write to be intact", got)
	}
}

// TestWorkspaceEditInitScript_KeepsTheTempFileWhenTheApplyFails mirrors
// `boid config edit`'s failure discipline: an edit that cannot be applied
// leaves the operator's work on disk with its path reported, rather than
// discarding it.
func TestWorkspaceEditInitScript_KeepsTheTempFileWhenTheApplyFails(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	// An "editor" that writes a script containing a NUL — rejected by the
	// client-side validation, so nothing reaches the daemon and the temp file
	// is all the operator has left.
	t.Setenv("EDITOR", editorScript(t, "printf 'echo hi\\000\\n' >"))

	var out bytes.Buffer
	err := runWorkspaceEditInitScript(newInitScriptCmd(t, workspaceEditInitScriptCmd, ts, &out), []string{"team-a"})
	if err == nil {
		t.Fatal("edit-init-script accepted a script containing a NUL byte")
	}
	kept := extractKeptPath(t, err.Error())
	if _, statErr := os.Stat(kept); statErr != nil {
		t.Fatalf("the error named %q as the kept edit, but it is not there: %v", kept, statErr)
	}
}

// TestWorkspaceEditInitScript_AppliesTheEdit is the success path.
func TestWorkspaceEditInitScript_AppliesTheEdit(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	t.Setenv("EDITOR", editorScript(t, "printf 'echo edited\\n' >"))

	var out bytes.Buffer
	if err := runWorkspaceEditInitScript(newInitScriptCmd(t, workspaceEditInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, err := os.ReadFile(daemonInitScriptPath(t, "team-a"))
	if err != nil {
		t.Fatalf("read daemon init.sh: %v", err)
	}
	if string(got) != "echo edited\n" {
		t.Errorf("daemon init.sh = %q, want %q", got, "echo edited\n")
	}
}

// TestWorkspaceEditInitScript_NoChangesIsANoOp: saving without editing must
// not bump the script's revision, which would re-run init for nothing.
func TestWorkspaceEditInitScript_NoChangesIsANoOp(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	workspaceInitScriptFile = writeTempScript(t, "echo hi\n")
	var setOut bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &setOut), []string{"team-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	t.Setenv("EDITOR", editorScript(t, "true #"))

	var out bytes.Buffer
	if err := runWorkspaceEditInitScript(newInitScriptCmd(t, workspaceEditInitScriptCmd, ts, &out), []string{"team-a"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(out.String(), "no changes") {
		t.Errorf("stdout = %q, want \"no changes\"", out.String())
	}
}

// TestWorkspaceInitScript_ExportApplyRoundTripRestoresTheScript is 論点 d's
// first half end to end: `workspace export` carries the script, and
// `workspace apply` of that document puts it back.
//
// This is the migration path the plan doc asks for — the five host-side
// workspaces are moved by exporting from one install and applying into
// another — so a round trip that loses the script would look like a working
// backup right up until the workspaces it restored could not start a harness.
func TestWorkspaceInitScript_ExportApplyRoundTripRestoresTheScript(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	script := "#!/bin/sh\nset -eu\nexport PATH=\"$HOME/.local/bin:$PATH\"\necho ready\n"
	workspaceInitScriptFile = writeTempScript(t, script)
	var setOut bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &setOut), []string{"team-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Export.
	exported := filepath.Join(t.TempDir(), "team-a.yaml")
	workspaceExportOutput = exported
	var exportOut bytes.Buffer
	if err := runWorkspaceExport(newInitScriptCmd(t, workspaceExportCmd, ts, &exportOut), []string{"team-a"}); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Lose the script, as a fresh install would not have it at all.
	var unsetOut bytes.Buffer
	if err := runWorkspaceUnsetInitScript(newInitScriptCmd(t, workspaceUnsetInitScriptCmd, ts, &unsetOut), []string{"team-a"}); err != nil {
		t.Fatalf("unset: %v", err)
	}

	// Apply the export back.
	if err := workspaceApplyCmd.Flags().Set("file", exported); err != nil {
		t.Fatalf("set apply -f: %v", err)
	}
	var applyOut bytes.Buffer
	if err := runWorkspaceApply(newInitScriptCmd(t, workspaceApplyCmd, ts, &applyOut), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := os.ReadFile(daemonInitScriptPath(t, "team-a"))
	if err != nil {
		t.Fatalf("init.sh was not restored by apply: %v", err)
	}
	if string(got) != script {
		t.Errorf("restored init.sh = %q, want %q", got, script)
	}
}

// TestWorkspaceApply_ReportsWhatItDidToTheInitScript keeps the script's fate
// out of the silent column.
//
// `workspace apply` already narrates every other side effect it has (created
// vs updated, each project attached or detached), and replacing a workspace's
// init.sh is a bigger one than any of those: it decides what the next dispatch
// installs into the HOME volume. Restoring a backup and being told only
// "workspace updated" hides the fact that the script changed underneath.
func TestWorkspaceApply_ReportsWhatItDidToTheInitScript(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	doc := writeApplyFile(t, "apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\nspec:\n  init_script: |\n    echo applied\n")
	if err := workspaceApplyCmd.Flags().Set("file", doc); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	// Dry run first: it must say what it WOULD do, and do nothing.
	if err := workspaceApplyCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set --dry-run: %v", err)
	}
	var dryOut bytes.Buffer
	if err := runWorkspaceApply(newInitScriptCmd(t, workspaceApplyCmd, ts, &dryOut), nil); err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	if !strings.Contains(dryOut.String(), "init.sh") || !strings.Contains(dryOut.String(), "would") {
		t.Errorf("--dry-run stdout = %q, want it to say what it would do to init.sh", dryOut.String())
	}
	if _, err := os.Stat(daemonInitScriptPath(t, "team-a")); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote the init script (stat err = %v)", err)
	}

	if err := workspaceApplyCmd.Flags().Set("dry-run", "false"); err != nil {
		t.Fatalf("reset --dry-run: %v", err)
	}
	var out bytes.Buffer
	if err := runWorkspaceApply(newInitScriptCmd(t, workspaceApplyCmd, ts, &out), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out.String(), "init.sh") {
		t.Errorf("stdout = %q, want it to report the init.sh write", out.String())
	}
}

// TestWorkspaceApply_AcceptsADocumentWithNoInitScriptKey is the other side of
// the export-side requirement added for PR9 codex round 4 (final) Major 1, and
// exists to keep the two from being "fixed" into agreement.
//
// `workspace export` now REFUSES a response whose documents carry no
// spec.init_script, because for an export the key's absence can only mean the
// daemon does not know about init.sh — an incomplete backup. `workspace apply`
// must keep ACCEPTING it: a hand-written document that adjusts, say, env alone
// legitimately omits the key, and the schema's own answer for an absent key is
// "leave this workspace's init.sh alone" (WorkspaceEnvelopeSpec.InitScript).
// Requiring it on both sides would turn every partial hand-written document
// into an error and force operators to paste their whole init.sh into a file
// that has nothing to do with it.
func TestWorkspaceApply_AcceptsADocumentWithNoInitScriptKey(t *testing.T) {
	ts := testutil.NewTestServer(t)
	resetInitScriptFlags(t)
	defer resetInitScriptFlags(t)
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)
	seedInitScriptWorkspace(t, ts, "team-a")

	workspaceInitScriptFile = writeTempScript(t, "echo original\n")
	var setOut bytes.Buffer
	if err := runWorkspaceSetInitScript(newInitScriptCmd(t, workspaceSetInitScriptCmd, ts, &setOut), []string{"team-a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	doc := writeApplyFile(t, "apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\nspec:\n  allowed_domains:\n    - example.invalid\n")
	if err := workspaceApplyCmd.Flags().Set("file", doc); err != nil {
		t.Fatalf("set -f: %v", err)
	}
	var out bytes.Buffer
	if err := runWorkspaceApply(newInitScriptCmd(t, workspaceApplyCmd, ts, &out), nil); err != nil {
		t.Fatalf("apply of a document with no init_script key must be accepted: %v", err)
	}

	got, err := os.ReadFile(daemonInitScriptPath(t, "team-a"))
	if err != nil {
		t.Fatalf("the apply removed an init.sh its document never mentioned: %v", err)
	}
	if string(got) != "echo original\n" {
		t.Errorf("init.sh = %q, want it left alone at %q", got, "echo original\n")
	}
	if strings.Contains(out.String(), "init.sh") {
		t.Errorf("stdout = %q, want no init.sh line for a document that did not carry the key", out.String())
	}
}

// editorScript writes a tiny shell script that stands in for $EDITOR: it runs
// `<body> "$1"`, so a caller passes something like "printf 'x\n' >" to
// overwrite the file, or "true #" to leave it alone.
func editorScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-editor.sh")
	content := "#!/bin/sh\n" + body + " \"$1\"\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	return path
}

// extractKeptPath pulls the "(edited init.sh kept at <path>)" location out of
// an error message.
func extractKeptPath(t *testing.T, msg string) string {
	t.Helper()
	const marker = "kept at "
	i := strings.Index(msg, marker)
	if i < 0 {
		t.Fatalf("error %q does not report where the edit was kept", msg)
	}
	rest := msg[i+len(marker):]
	end := strings.IndexAny(rest, " )\n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

package cmd

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil"
)

// seedHostCommandsForTest hand-writes the aggregated host_commands.yaml and
// reloads it into the running daemon, so a subsequent workspace create/
// edit/assign body referencing one of names passes MAJOR 2's live-snapshot
// validation (docs/plans/workspace-db-consolidation.md, codex review). Tests
// below reference "gh" purely as CRUD-flow filler content, not testing
// host_commands validation itself.
func seedHostCommandsForTest(t *testing.T, ts *testutil.TestServer, names ...string) {
	t.Helper()
	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	specs := make(map[string]orchestrator.HostCommandSpec, len(names))
	for _, name := range names {
		specs[name] = orchestrator.HostCommandSpec{}
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, specs); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}
	if err := ts.Server.ReloadHostCommands(); err != nil {
		t.Fatalf("ReloadHostCommands: %v", err)
	}
}

// TestFormatStringSlice verifies the helper that formats kit/slug lists.
func TestFormatStringSlice(t *testing.T) {
	if got := formatStringSlice(nil); got != "(none)" {
		t.Errorf("nil: got %q, want \"(none)\"", got)
	}
	if got := formatStringSlice([]string{}); got != "(none)" {
		t.Errorf("empty: got %q, want \"(none)\"", got)
	}
	if got := formatStringSlice([]string{"a", "b"}); got != "a, b" {
		t.Errorf("multi: got %q, want \"a, b\"", got)
	}
}

// TestWorkspaceRemove_RejectsDefault verifies the CLI-layer guard that
// stops `boid workspace remove default` before any DB or filesystem
// modification. The domain-layer guard (WorkspaceStore.Remove) is the
// last line of defense; this is the first.
func TestWorkspaceRemove_RejectsDefault(t *testing.T) {
	cmd := workspaceRemoveCmd
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	err := runWorkspaceRemove(cmd, []string{orchestrator.DefaultWorkspaceSlug})
	if err == nil {
		t.Fatal("expected error rejecting default workspace, got nil")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("expected 'reserved' in error message, got %q", err.Error())
	}
}

// TestWorkspaceRemove_SlugValidation verifies that invalid slugs are rejected.
func TestWorkspaceRemove_SlugValidation(t *testing.T) {
	cases := []struct {
		slug    string
		wantErr bool
	}{
		{"valid-slug", false},
		{"", true},
		{"UPPER", true},
		{"with space", true},
		{strings.Repeat("x", 65), true},
	}
	for _, tc := range cases {
		err := orchestrator.ValidWorkspaceSlug(tc.slug)
		if tc.wantErr && err == nil {
			t.Errorf("slug %q: expected error", tc.slug)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("slug %q: unexpected error: %v", tc.slug, err)
		}
	}
}

// ---------------------------------------------------------------------------
// buildWorkspaceCreateBody / formatWorkspaceAPIError (PR4 Step H helpers)
// ---------------------------------------------------------------------------

func TestBuildWorkspaceCreateBody_EmptyMeta(t *testing.T) {
	body, err := buildWorkspaceCreateBody("team-a", nil)
	if err != nil {
		t.Fatalf("buildWorkspaceCreateBody: %v", err)
	}
	slug, meta, err := orchestrator.DecodeWorkspaceCreateStrict(body)
	if err != nil {
		t.Fatalf("DecodeWorkspaceCreateStrict round-trip: %v", err)
	}
	if slug != "team-a" {
		t.Errorf("slug = %q, want team-a", slug)
	}
	if len(meta.HostCommands) != 0 {
		t.Errorf("expected empty meta, got %+v", meta)
	}
}

func TestBuildWorkspaceCreateBody_MergesFromFileContent(t *testing.T) {
	fromFile := []byte("host_commands:\n  - gh\nenv:\n  FOO: bar\n")
	body, err := buildWorkspaceCreateBody("team-a", fromFile)
	if err != nil {
		t.Fatalf("buildWorkspaceCreateBody: %v", err)
	}
	slug, meta, err := orchestrator.DecodeWorkspaceCreateStrict(body)
	if err != nil {
		t.Fatalf("DecodeWorkspaceCreateStrict round-trip: %v", err)
	}
	if slug != "team-a" {
		t.Errorf("slug = %q, want team-a", slug)
	}
	if len(meta.HostCommands) != 1 || meta.HostCommands[0] != "gh" {
		t.Errorf("HostCommands = %v, want [gh]", meta.HostCommands)
	}
	if meta.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want bar", meta.Env["FOO"])
	}
}

func TestBuildWorkspaceCreateBody_RejectsInvalidYAML(t *testing.T) {
	_, err := buildWorkspaceCreateBody("team-a", []byte("not: [valid"))
	if err == nil {
		t.Fatal("expected error for invalid --from-file yaml, got nil")
	}
}

// TestBuildWorkspaceCreateBody_RejectsMultipleDocuments pins the codex
// 4th-pass fix: buildWorkspaceCreateBody sits on the create/configure
// paths (runWorkspaceCreate → this, syncWorkspaceYAMLToDB → this via
// workspace configure), and prior to the fix its plain yaml.Unmarshal
// silently dropped everything past the first `---` document — the
// server's DecodeWorkspaceCreateStrict never saw the trailing document
// and multi-document rejection was defeated for these two entry points.
// Now the strict decoder runs on the raw --from-file bytes first, so a
// two-document body is rejected before it ever becomes a POST.
func TestBuildWorkspaceCreateBody_RejectsMultipleDocuments(t *testing.T) {
	twoDocs := []byte("host_commands: [gh]\n---\nhost_commands: [aws]\n")
	_, err := buildWorkspaceCreateBody("team-a", twoDocs)
	if err == nil {
		t.Fatal("expected error for multi-document --from-file yaml, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "parse --from-file") {
		t.Errorf("error message %q should mention parse --from-file", err.Error())
	}
}

func TestFormatWorkspaceAPIError_ExtractsErrorField(t *testing.T) {
	body := []byte(`{"error":"revision mismatch"}`)
	got := formatWorkspaceAPIError(http.StatusPreconditionFailed, body)
	if !strings.Contains(got, "revision mismatch") {
		t.Errorf("got %q, want it to mention the error field", got)
	}
	if !strings.Contains(got, "412") {
		t.Errorf("got %q, want it to mention the status code", got)
	}
}

func TestFormatWorkspaceAPIError_FallsBackToStatusCode(t *testing.T) {
	got := formatWorkspaceAPIError(http.StatusInternalServerError, []byte("not json"))
	if !strings.Contains(got, "500") {
		t.Errorf("got %q, want it to mention the status code", got)
	}
}

// ---------------------------------------------------------------------------
// Integration tests against a real daemon (testutil.NewTestServer)
// ---------------------------------------------------------------------------

// resetWorkspaceCreateEditFlags clears the package-level --from-file/--force
// flag state that workspaceCreateCmd/workspaceEditCmd bind to, so tests
// running against the shared command vars do not leak flag values into each
// other (these are package-level *cobra.Command singletons, matching this
// file's existing pattern of reusing workspaceRemoveCmd/workspaceConfigureCmd
// directly rather than constructing fresh instances per test).
func resetWorkspaceCreateEditFlags(t *testing.T) {
	t.Helper()
	if err := workspaceCreateCmd.Flags().Set("from-file", ""); err != nil {
		t.Fatalf("reset create --from-file: %v", err)
	}
	if err := workspaceEditCmd.Flags().Set("from-file", ""); err != nil {
		t.Fatalf("reset edit --from-file: %v", err)
	}
	if err := workspaceEditCmd.Flags().Set("force", "false"); err != nil {
		t.Fatalf("reset edit --force: %v", err)
	}
}

func TestRunWorkspaceList_UsesAPIOnly(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	testutil.SeedWorkspace(t, ts, "team-a")

	var out bytes.Buffer
	cmd := workspaceListCmd
	cmd.SetOut(&out)
	if err := runWorkspaceList(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "team-a") {
		t.Errorf("expected team-a in output, got %q", got)
	}
	if !strings.Contains(got, orchestrator.DefaultWorkspaceSlug) {
		t.Errorf("expected default workspace in output, got %q", got)
	}
}

func TestRunWorkspaceCreateShowEditRemove_FullCycle(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceCreateEditFlags(t)

	// create (empty).
	var createOut bytes.Buffer
	cmd := workspaceCreateCmd
	cmd.SetOut(&createOut)
	if err := runWorkspaceCreate(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceCreate: %v", err)
	}
	if !strings.Contains(createOut.String(), "team-a") {
		t.Errorf("create output = %q", createOut.String())
	}

	// show.
	var showOut bytes.Buffer
	showCmd := workspaceShowCmd
	showCmd.SetOut(&showOut)
	if err := runWorkspaceShow(showCmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceShow: %v", err)
	}
	if !strings.Contains(showOut.String(), "team-a") {
		t.Errorf("show output = %q", showOut.String())
	}

	// edit --from-file (auto If-Match).
	editFile := filepath.Join(t.TempDir(), "edit.yaml")
	if err := os.WriteFile(editFile, []byte("host_commands:\n  - gh\n"), 0o644); err != nil {
		t.Fatalf("write edit file: %v", err)
	}
	if err := workspaceEditCmd.Flags().Set("from-file", editFile); err != nil {
		t.Fatalf("set --from-file: %v", err)
	}
	var editOut bytes.Buffer
	editCmd := workspaceEditCmd
	editCmd.SetOut(&editOut)
	if err := runWorkspaceEdit(editCmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceEdit: %v", err)
	}
	if !strings.Contains(editOut.String(), "team-a") {
		t.Errorf("edit output = %q", editOut.String())
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &detail); err != nil {
		t.Fatalf("verify edit: %v", err)
	}
	if len(detail.Meta.HostCommands) != 1 || detail.Meta.HostCommands[0] != "gh" {
		t.Errorf("HostCommands after edit = %v, want [gh]", detail.Meta.HostCommands)
	}

	// remove.
	var removeOut bytes.Buffer
	removeCmd := workspaceRemoveCmd
	removeCmd.SetOut(&removeOut)
	if err := runWorkspaceRemove(removeCmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceRemove: %v", err)
	}

	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &api.WorkspaceDetail{}); err == nil {
		t.Fatal("expected team-a to be gone after remove")
	}
}

// TestRunWorkspaceAssign_AutoCreatesFromLocalYAML pins the PR4 Step H
// behavior change: assigning a project to a slug with no DB row yet, but a
// legacy local workspace.yaml, must auto-create the DB row from that yaml
// (ensureWorkspaceExistsForAssign) so the reinstated existence check (Step
// J) does not break the existing "drop a yaml file, then `boid workspace
// assign`" e2e flow.
func TestRunWorkspaceAssign_AutoCreatesFromLocalYAML(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")

	// Drop a local workspace.yaml directly (yaml-mode store, matches how an
	// e2e scenario or `boid workspace configure` would leave one behind —
	// neither creates a DB row).
	yamlStore := orchestrator.NewWorkspaceStore("")
	if err := yamlStore.Save("legacy-ws", &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("seed local workspace.yaml: %v", err)
	}

	// No DB row yet.
	if err := ts.Client.Do("GET", "/api/workspaces/legacy-ws", nil, &api.WorkspaceDetail{}); err == nil {
		t.Fatal("expected legacy-ws to have no DB row before assign")
	}

	dir := writeImportTestProject(t, "assign-proj", "Assign Proj")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runWorkspaceAssign(cmd, []string{project.ID, "legacy-ws"}); err != nil {
		t.Fatalf("runWorkspaceAssign: %v", err)
	}

	var updated orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/"+project.ID, nil, &updated); err != nil {
		t.Fatalf("get project after assign: %v", err)
	}
	if updated.WorkspaceID != "legacy-ws" {
		t.Errorf("WorkspaceID = %q, want legacy-ws", updated.WorkspaceID)
	}

	// The DB row now exists, carrying the legacy yaml's content.
	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/legacy-ws", nil, &detail); err != nil {
		t.Fatalf("expected legacy-ws to now have a DB row: %v", err)
	}
	if len(detail.Meta.HostCommands) != 1 || detail.Meta.HostCommands[0] != "gh" {
		t.Errorf("auto-created workspace HostCommands = %v, want [gh]", detail.Meta.HostCommands)
	}
}

// TestRunWorkspaceAssign_UnknownSlugNoYAMLStill404s verifies the other half
// of Step J/H: a slug with neither a DB row nor a local yaml must still
// 404 on assign (no silent get-or-create for a genuinely unknown slug).
func TestRunWorkspaceAssign_UnknownSlugNoYAMLStill404s(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	dir := writeImportTestProject(t, "assign-proj-2", "Assign Proj 2")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWorkspaceAssign(cmd, []string{project.ID, "totally-unknown"})
	if err == nil {
		t.Fatal("expected error assigning to a slug with no DB row and no local yaml")
	}
}

// TestRunWorkspaceAssign_LocalYAMLParseErrorSurfaces pins MINOR 3-b (codex
// review, docs/plans/workspace-db-consolidation.md):
// ensureWorkspaceExistsForAssign's auto-create pre-check used to swallow
// *any* local workspace.yaml read failure — including a parse error or a
// permission error, not just "file does not exist" — and silently fall
// through to "no local yaml either", so a genuine config problem only ever
// surfaced as a confusing 404 from the subsequent assign call. A malformed
// workspace.yaml must now surface its own parse error directly instead.
func TestRunWorkspaceAssign_LocalYAMLParseErrorSurfaces(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		t.Fatalf("DefaultWorkspaceDir: %v", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	// Deliberately broken YAML (matches TestWorkspaceStore_LoadParseError's
	// fixture): an unclosed bracket.
	badYAML := []byte("kits: [unclosed bracket\n")
	if err := os.WriteFile(filepath.Join(wsDir, "broken-ws.yaml"), badYAML, 0o644); err != nil {
		t.Fatalf("write broken workspace.yaml: %v", err)
	}

	dir := writeImportTestProject(t, "assign-proj-3", "Assign Proj 3")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err = runWorkspaceAssign(cmd, []string{project.ID, "broken-ws"})
	if err == nil {
		t.Fatal("expected the local workspace.yaml parse error to surface, got nil")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("error must report the parse failure, not a generic 'not found': %v", err)
	}
}

// multiDocWorkspaceYAML is a minimal two-document yaml fixture shared by the
// MINOR 1 tests below (codex review round 3, docs/plans/
// workspace-db-consolidation.md): a caller who hand-authors this (e.g. a
// copy-paste mistake) must have it rejected, not silently truncated to the
// first document.
const multiDocWorkspaceYAML = "env:\n  FOO: bar\n---\nenv:\n  FOO: baz\n"

// TestRunWorkspaceCreate_RejectsMultipleDocuments pins MINOR 1 (codex review
// round 3): `boid workspace create --from-file` used to read --from-file
// with a plain (non-strict) yaml.Unmarshal into a map[string]any and
// re-marshal a single document from it before ever reaching the server —
// silently dropping a second "---"-delimited document, so the server's own
// strict multi-document reject (DecodeWorkspaceCreateStrict) never got a
// chance to see it. No daemon is reachable in this test at all: the
// validation must fail client-side before any HTTP call is attempted.
func TestRunWorkspaceCreate_RejectsMultipleDocuments(t *testing.T) {
	resetWorkspaceCreateEditFlags(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(file, []byte(multiDocWorkspaceYAML), 0o644); err != nil {
		t.Fatalf("write multi-doc yaml: %v", err)
	}
	if err := workspaceCreateCmd.Flags().Set("from-file", file); err != nil {
		t.Fatalf("set --from-file: %v", err)
	}
	t.Setenv("BOID_SOCKET", filepath.Join(dir, "no-daemon-here.sock"))

	cmd := workspaceCreateCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWorkspaceCreate(cmd, []string{"team-a"})
	if err == nil {
		t.Fatal("expected an error rejecting the multi-document --from-file")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("expected a multi-document rejection error, got: %v", err)
	}
}

// TestRunWorkspaceEdit_RejectsMultipleDocuments is the `workspace edit`
// counterpart of the create test above. --force is set so the command skips
// its automatic revision GET, isolating the assertion to the --from-file
// validation itself: without the client-side check, this would instead fail
// with a connection error against the unreachable socket, not a
// multi-document rejection.
func TestRunWorkspaceEdit_RejectsMultipleDocuments(t *testing.T) {
	resetWorkspaceCreateEditFlags(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(file, []byte(multiDocWorkspaceYAML), 0o644); err != nil {
		t.Fatalf("write multi-doc yaml: %v", err)
	}
	if err := workspaceEditCmd.Flags().Set("from-file", file); err != nil {
		t.Fatalf("set --from-file: %v", err)
	}
	if err := workspaceEditCmd.Flags().Set("force", "true"); err != nil {
		t.Fatalf("set --force: %v", err)
	}
	t.Setenv("BOID_SOCKET", filepath.Join(dir, "no-daemon-here.sock"))

	cmd := workspaceEditCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWorkspaceEdit(cmd, []string{"team-a"})
	if err == nil {
		t.Fatal("expected an error rejecting the multi-document --from-file")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("expected a multi-document rejection error, got: %v", err)
	}
}

// TestRunWorkspaceAssign_AutoCreate_RejectsMultipleDocuments pins MINOR 1's
// third vector: `boid workspace assign`'s auto-create pre-check
// (ensureWorkspaceExistsForAssign) used to read a local workspace.yaml via
// WorkspaceStore.Load's plain (non-strict) yaml.Unmarshal, which silently
// drops a second document — the resulting (already-truncated) meta was then
// re-marshaled and POSTed successfully, so the multi-document mistake never
// surfaced as an error anywhere; the assign would just quietly succeed using
// only the first document.
func TestRunWorkspaceAssign_AutoCreate_RejectsMultipleDocuments(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		t.Fatalf("DefaultWorkspaceDir: %v", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "multi-ws.yaml"), []byte(multiDocWorkspaceYAML), 0o644); err != nil {
		t.Fatalf("write multi-doc workspace.yaml: %v", err)
	}

	dir := writeImportTestProject(t, "assign-proj-4", "Assign Proj 4")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err = runWorkspaceAssign(cmd, []string{project.ID, "multi-ws"})
	if err == nil {
		t.Fatal("expected the multi-document local workspace.yaml to be rejected, got nil (silently auto-created from the truncated first document)")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("expected a multi-document rejection error, got: %v", err)
	}
}

// TestExtractLegacyWorkspaceKitRefs_RejectsMultipleDocuments pins MAJOR 3
// (codex review round 1, docs/plans/workspace-db-consolidation.md): a
// multi-document local workspace.yaml whose *first* document contains a
// `kits:` key must still be rejected, not silently truncated. Before the
// fix, extractLegacyWorkspaceKitRefs unmarshaled raw into a map[string]any
// (a plain single-document yaml.Unmarshal, which always drops any trailing
// "---"-delimited document with no error) and then, whenever `kits:` was
// present, re-marshaled that already-truncated map as `rest` — so the
// caller's later orchestrator.DecodeWorkspaceMetaStrict(rest) call could
// never observe the dropped second document, since rest was a fresh marshal
// of only the first document's fields. (The counterpart test above,
// TestRunWorkspaceAssign_AutoCreate_RejectsMultipleDocuments, exercises a
// fixture with NO kits: key — which happened to already work, because `rest`
// there was the original unmodified raw bytes, not a re-marshal.)
func TestExtractLegacyWorkspaceKitRefs_RejectsMultipleDocuments(t *testing.T) {
	twoDocsWithKits := []byte("kits:\n  - kit-a\n---\nkits:\n  - kit-b\n")
	_, _, err := extractLegacyWorkspaceKitRefs(twoDocsWithKits)
	if err == nil {
		t.Fatal("expected an error for a multi-document yaml whose first document has a kits: key, got nil")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("expected a multi-document rejection error, got: %v", err)
	}
}

// TestExtractLegacyWorkspaceKitRefs_SingleDocumentStillWorks is the negative
// counterpart of the above: a well-formed single-document yaml with a kits:
// key must still extract cleanly (no regression from the added trailing-
// document check).
func TestExtractLegacyWorkspaceKitRefs_SingleDocumentStillWorks(t *testing.T) {
	kitRefs, rest, err := extractLegacyWorkspaceKitRefs([]byte("kits:\n  - kit-a\nenv:\n  FOO: bar\n"))
	if err != nil {
		t.Fatalf("extractLegacyWorkspaceKitRefs: %v", err)
	}
	if len(kitRefs) != 1 || kitRefs[0] != "kit-a" {
		t.Errorf("kitRefs = %v, want [kit-a]", kitRefs)
	}
	if strings.Contains(string(rest), "kits") {
		t.Errorf("rest still contains a kits key: %s", rest)
	}
	if !strings.Contains(string(rest), "FOO") {
		t.Errorf("rest lost the env field: %s", rest)
	}
}

// TestExtractLegacyWorkspaceKitRefs_NullOrEmptyKitsIsAcceptedAsNoRefs pins
// codex PR7 review round 3's minor finding: `kits: null` and a bare `kits:`
// with no value both parse to nil under yaml.v3 map decoding — the loose
// pre-PR7 shadow yaml path treated them as "no refs, key present". A prior
// draft of the hardening asserted `.([]any)` directly and 500'd on nil,
// silently breaking `boid workspace assign` for existing shadow yaml files
// that legitimately carried `kits:` with nothing under it. Both forms must
// be accepted as the same "no legacy kit refs" state that key-absent yields,
// and the kits: null line must be scrubbed from the outgoing body so the
// downstream strict decoder does not choke on it either.
func TestExtractLegacyWorkspaceKitRefs_NullOrEmptyKitsIsAcceptedAsNoRefs(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"kits: null", "kits: null\nenv:\n  FOO: bar\n"},
		{"bare kits key with no value", "kits:\nenv:\n  FOO: bar\n"},
		{"empty kits list", "kits: []\nenv:\n  FOO: bar\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kitRefs, rest, err := extractLegacyWorkspaceKitRefs([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("extractLegacyWorkspaceKitRefs(%q): %v", tc.name, err)
			}
			if len(kitRefs) != 0 {
				t.Errorf("kitRefs = %v, want empty (nil/null/[] should mean no refs)", kitRefs)
			}
			if strings.Contains(string(rest), "kits") {
				t.Errorf("rest still contains a kits key (should be scrubbed): %s", rest)
			}
			if !strings.Contains(string(rest), "FOO") {
				t.Errorf("rest lost the env field: %s", rest)
			}
		})
	}
}

// TestRunWorkspaceAssign_AutoCreate_FailsOnUnresolvedKit pins MAJOR 2 (codex
// review round 1, docs/plans/workspace-db-consolidation.md): a local
// workspace.yaml referencing a kit with no corresponding kit.yaml must fail
// `boid workspace assign`'s auto-create step outright, rather than silently
// downgrading to a "note:" warning and creating the workspace without the
// kit's host_commands/env/bindings — a regression from PR6, where the
// daemon-side CreateWorkspace 400'd on the very same condition and `assign`
// itself exited non-zero. The workspace must end up with no DB row at all:
// MaterializeWorkspaceKitsForPersist fails before postWorkspaceCreateBestEffort
// (the actual POST /api/workspaces call) is ever reached.
//
// This constructs its own server (rather than testutil.NewTestServer) with a
// real, non-empty --kits-dir that deliberately lacks "ghost-kit" — MAJOR 2
// (codex review round 2) made resolveDaemonKitsDir hard-error on an empty
// daemon KitsDir (testutil.NewTestServer's default) instead of falling back
// to this CLI process's own defaultKitsDir(), so this test needs a daemon
// that actually answers GET /api/config/kits-dir with a real directory in
// order to reach (and pin) the unresolved-kit failure this test is about,
// rather than failing one step earlier on the kits-dir lookup itself.
func TestRunWorkspaceAssign_AutoCreate_FailsOnUnresolvedKit(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	kitsDir := t.TempDir() // the daemon's real --kits-dir, deliberately WITHOUT "ghost-kit".

	sockPath := filepath.Join(t.TempDir(), "boid.sock")
	t.Setenv("BOID_SOCKET", sockPath)

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    kitsDir,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	c := client.NewUnixClient(sockPath)

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		t.Fatalf("DefaultWorkspaceDir: %v", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "ghost-ws.yaml"), []byte("kits:\n  - ghost-kit\n"), 0o644); err != nil {
		t.Fatalf("write workspace.yaml: %v", err)
	}

	dir := writeImportTestProject(t, "assign-proj-ghost", "Assign Proj Ghost")
	var project orchestrator.Project
	if err := c.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err = runWorkspaceAssign(cmd, []string{project.ID, "ghost-ws"})
	if err == nil {
		t.Fatal("expected an error assigning to a workspace whose local yaml references an unresolved kit, got nil")
	}
	if !strings.Contains(err.Error(), "ghost-kit") {
		t.Errorf("expected the error to mention the unresolved kit name %q: %v", "ghost-kit", err)
	}

	// The workspace must not have been created at all.
	if err := c.Do("GET", "/api/workspaces/ghost-ws", nil, &api.WorkspaceDetail{}); err == nil {
		t.Error("expected ghost-ws to have no DB row after the unresolved-kit assign failure")
	}
}

// TestRunWorkspaceAssign_AutoCreate_HonorsCustomKitsDir pins MAJOR 1 (codex
// review round 1, docs/plans/workspace-db-consolidation.md): a real
// regression where ensureWorkspaceExistsForAssign's client-side kit
// materialization called defaultKitsDir() unconditionally — silently
// ignoring a daemon started with `boid start --kits-dir <custom>` and
// resolving kit references against the wrong (CLI-local-default) directory
// instead. testutil.NewTestServer does not accept a custom KitsDir, so this
// test constructs a bespoke server directly (mirrors cmd/project_migrate_test.go's
// TestProjectMigrate_WithHostCommandsAndBindings for the same reason). The
// referenced kit is placed ONLY under the server's custom KitsDir, never
// under what this test's own ambient $XDG_DATA_HOME would make
// defaultKitsDir() resolve to — if resolveDaemonKitsDir ever regresses back
// to defaultKitsDir(), this kit lookup 404s and the assign fails.
func TestRunWorkspaceAssign_AutoCreate_HonorsCustomKitsDir(t *testing.T) {
	cfgDir := t.TempDir()
	dataDir := t.TempDir()      // defaultKitsDir() resolves under here — deliberately WITHOUT the kit.
	customKitsDir := t.TempDir() // the daemon's actual --kits-dir — WITH the kit.

	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	kitDir := filepath.Join(customKitsDir, "custom-kit")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("host_commands:\n  custom-cmd:\n    allow: [\"*\"]\n"), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}

	sockPath := filepath.Join(t.TempDir(), "boid.sock")
	t.Setenv("BOID_SOCKET", sockPath)

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    customKitsDir,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	c := client.NewUnixClient(sockPath)

	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, map[string]orchestrator.HostCommandSpec{
		"custom-cmd": {},
	}); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}
	if err := srv.ReloadHostCommands(); err != nil {
		t.Fatalf("ReloadHostCommands: %v", err)
	}

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		t.Fatalf("DefaultWorkspaceDir: %v", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "custom-kits-ws.yaml"), []byte("kits:\n  - custom-kit\n"), 0o644); err != nil {
		t.Fatalf("write workspace.yaml: %v", err)
	}

	dir := writeImportTestProject(t, "assign-proj-customkits", "Assign Proj CustomKits")
	var project orchestrator.Project
	if err := c.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runWorkspaceAssign(cmd, []string{project.ID, "custom-kits-ws"}); err != nil {
		t.Fatalf("runWorkspaceAssign: %v (want success — the kit should resolve via the daemon's actual --kits-dir)", err)
	}

	var detail api.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/custom-kits-ws", nil, &detail); err != nil {
		t.Fatalf("verify auto-created workspace: %v", err)
	}
	if len(detail.Meta.HostCommands) != 1 || detail.Meta.HostCommands[0] != "custom-cmd" {
		t.Errorf("HostCommands = %v, want [custom-cmd] (kit resolved from the daemon's custom --kits-dir, not the CLI's own defaultKitsDir())", detail.Meta.HostCommands)
	}
}

// ---------------------------------------------------------------------------
// Export / Import (docs/plans/workspace-db-consolidation.md PR5)
// ---------------------------------------------------------------------------

// resetWorkspaceExportImportFlags clears the package-level flag state
// workspaceExportCmd/workspaceImportCmd bind to, mirroring
// resetWorkspaceCreateEditFlags above for the same reason (shared
// package-level *cobra.Command singletons).
//
// Setting a flag's value via .Set(...) also flips its .Changed=true, which
// leaks across tests: runWorkspaceImport's --force/--mode conflict check
// (codex PR5 review, minor: silent --force → replace override) reads
// .Changed to know whether the caller explicitly typed --mode. Clearing
// .Changed=false after the .Set(...) restores a pristine "as if never
// typed" state, so a later test that leaves --mode at its default and
// only sets --force gets the alias behaviour, not the conflict.
func resetWorkspaceExportImportFlags(t *testing.T) {
	t.Helper()
	if err := workspaceExportCmd.Flags().Set("output", ""); err != nil {
		t.Fatalf("reset export --output: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("mode", "create-only"); err != nil {
		t.Fatalf("reset import --mode: %v", err)
	}
	workspaceImportCmd.Flags().Lookup("mode").Changed = false
	if err := workspaceImportCmd.Flags().Set("force", "false"); err != nil {
		t.Fatalf("reset import --force: %v", err)
	}
	workspaceImportCmd.Flags().Lookup("force").Changed = false
	if err := workspaceImportCmd.Flags().Set("slug", ""); err != nil {
		t.Fatalf("reset import --slug: %v", err)
	}
	workspaceImportCmd.Flags().Lookup("slug").Changed = false
	workspaceExportCmd.Flags().Lookup("output").Changed = false
}

// TestRunWorkspaceExport_StdoutRoundTrip pins PR5 Step D/A: exporting a
// workspace to stdout must produce a yaml body that carries the top-level
// "slug:" key (matching CreateWorkspace/import input shape) so it can be
// piped straight back into `boid workspace import` without any translation
// step (codex PR5 review, MAJOR: round-trip 非対称は避ける).
func TestRunWorkspaceExport_StdoutRoundTrip(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceCreateEditFlags(t)
	resetWorkspaceExportImportFlags(t)

	if err := runWorkspaceCreate(workspaceCreateCmd, []string{"team-a"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	editFile := filepath.Join(t.TempDir(), "edit.yaml")
	if err := os.WriteFile(editFile, []byte("host_commands:\n  - gh\n"), 0o644); err != nil {
		t.Fatalf("write edit file: %v", err)
	}
	if err := workspaceEditCmd.Flags().Set("from-file", editFile); err != nil {
		t.Fatalf("set --from-file: %v", err)
	}
	if err := runWorkspaceEdit(workspaceEditCmd, []string{"team-a"}); err != nil {
		t.Fatalf("seed edit: %v", err)
	}
	resetWorkspaceCreateEditFlags(t)

	var out bytes.Buffer
	cmd := workspaceExportCmd
	cmd.SetOut(&out)
	if err := runWorkspaceExport(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceExport: %v", err)
	}

	exported := out.Bytes()
	// The exported body carries the top-level slug key — the round-trip
	// symmetry that lets `boid workspace export team-a | boid workspace
	// import` work as-is.
	if !strings.Contains(string(exported), "slug: team-a") {
		t.Errorf("exported body must contain 'slug: team-a' at top level: %s", exported)
	}

	// The exported body IS a valid import body: DecodeWorkspaceCreateStrict
	// (the same decoder POST /api/workspaces/import uses) reconstructs slug
	// and meta directly.
	slug, meta, err := orchestrator.DecodeWorkspaceCreateStrict(exported)
	if err != nil {
		t.Fatalf("DecodeWorkspaceCreateStrict round-trip: %v", err)
	}
	if slug != "team-a" {
		t.Errorf("slug = %q, want team-a (from export body)", slug)
	}
	if !equalStrSliceForWorkspaceTest(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh] (lost across export/import round trip)", meta.HostCommands)
	}
}

// TestRunWorkspaceExport_OutputFile pins the --output flag: the exported
// yaml must be written to the given file path, not stdout, and stdout must
// stay empty.
func TestRunWorkspaceExport_OutputFile(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceCreateEditFlags(t)
	resetWorkspaceExportImportFlags(t)

	createBody, err := buildWorkspaceCreateBody("team-a", []byte("host_commands:\n  - gh\n"))
	if err != nil {
		t.Fatalf("buildWorkspaceCreateBody: %v", err)
	}
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", createBody, &api.WorkspaceDetail{}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	outFile := filepath.Join(t.TempDir(), "exported.yaml")
	if err := workspaceExportCmd.Flags().Set("output", outFile); err != nil {
		t.Fatalf("set --output: %v", err)
	}
	defer resetWorkspaceExportImportFlags(t)

	var out bytes.Buffer
	cmd := workspaceExportCmd
	cmd.SetOut(&out)
	if err := runWorkspaceExport(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceExport: %v", err)
	}

	if out.Len() == 0 {
		t.Error("expected a confirmation message on stdout")
	}
	fileData, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read --output file: %v", err)
	}
	if !strings.Contains(string(fileData), "gh") {
		t.Errorf("--output file content = %q, want it to mention gh", fileData)
	}
	// The --output file must carry the top-level slug key (round-trip
	// symmetry — codex PR5 review, MAJOR).
	if !strings.Contains(string(fileData), "slug: team-a") {
		t.Errorf("--output file must contain 'slug: team-a' at top level: %s", fileData)
	}
}

// TestRunWorkspaceExport_404OnMissing pins that exporting an unknown slug
// surfaces an error rather than writing an empty file/stdout silently.
func TestRunWorkspaceExport_404OnMissing(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := workspaceExportCmd
	cmd.SetOut(&out)
	err := runWorkspaceExport(cmd, []string{"ghost"})
	if err == nil {
		t.Fatal("expected an error exporting an unknown workspace, got nil")
	}
}

// TestRunWorkspaceImport_CreateOnlyMode pins the safe default: importing a
// brand-new slug succeeds, and importing the same slug again (still
// create-only, the default) 409s rather than silently overwriting.
func TestRunWorkspaceImport_CreateOnlyMode(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)

	importFile := filepath.Join(t.TempDir(), "team-c.yaml")
	if err := os.WriteFile(importFile, []byte("host_commands:\n  - gh\n"), 0o644); err != nil {
		t.Fatalf("write import file: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("slug", "team-c"); err != nil {
		t.Fatalf("set --slug: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceImportCmd
	cmd.SetOut(&out)
	if err := runWorkspaceImport(cmd, []string{importFile}); err != nil {
		t.Fatalf("runWorkspaceImport (first, should create): %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-c", nil, &detail); err != nil {
		t.Fatalf("verify import: %v", err)
	}
	if !equalStrSliceForWorkspaceTest(detail.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", detail.Meta.HostCommands)
	}

	// Second import of the same slug, still create-only (the default), must
	// fail rather than silently overwrite.
	var out2 bytes.Buffer
	cmd2 := workspaceImportCmd
	cmd2.SetOut(&out2)
	err := runWorkspaceImport(cmd2, []string{importFile})
	if err == nil {
		t.Fatal("expected an error re-importing an existing slug with mode=create-only, got nil")
	}
	if !strings.Contains(err.Error(), "409") && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		t.Errorf("expected a conflict error, got: %v", err)
	}
}

// TestRunWorkspaceImport_ReplaceMode pins --mode replace's upsert semantics:
// re-importing an existing slug must succeed and overwrite wholesale.
func TestRunWorkspaceImport_ReplaceMode(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh", "aws")
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)

	createBody, err := buildWorkspaceCreateBody("team-d", []byte("host_commands:\n  - gh\n"))
	if err != nil {
		t.Fatalf("buildWorkspaceCreateBody: %v", err)
	}
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", createBody, &api.WorkspaceDetail{}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	importFile := filepath.Join(t.TempDir(), "team-d.yaml")
	if err := os.WriteFile(importFile, []byte("host_commands:\n  - aws\n"), 0o644); err != nil {
		t.Fatalf("write import file: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("mode", "replace"); err != nil {
		t.Fatalf("set --mode replace: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("slug", "team-d"); err != nil {
		t.Fatalf("set --slug: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceImportCmd
	cmd.SetOut(&out)
	if err := runWorkspaceImport(cmd, []string{importFile}); err != nil {
		t.Fatalf("runWorkspaceImport --mode replace: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-d", nil, &detail); err != nil {
		t.Fatalf("verify import: %v", err)
	}
	if !equalStrSliceForWorkspaceTest(detail.Meta.HostCommands, []string{"aws"}) {
		t.Errorf("HostCommands = %v, want [aws] (replace must overwrite wholesale)", detail.Meta.HostCommands)
	}
}

// TestRunWorkspaceImport_ForceFlagIsReplaceAlias pins --force as a shorthand
// for --mode replace.
func TestRunWorkspaceImport_ForceFlagIsReplaceAlias(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)

	createBody, err := buildWorkspaceCreateBody("team-e", nil)
	if err != nil {
		t.Fatalf("buildWorkspaceCreateBody: %v", err)
	}
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", createBody, &api.WorkspaceDetail{}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	importFile := filepath.Join(t.TempDir(), "team-e.yaml")
	if err := os.WriteFile(importFile, []byte("host_commands:\n  - gh\n"), 0o644); err != nil {
		t.Fatalf("write import file: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("force", "true"); err != nil {
		t.Fatalf("set --force: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("slug", "team-e"); err != nil {
		t.Fatalf("set --slug: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceImportCmd
	cmd.SetOut(&out)
	if err := runWorkspaceImport(cmd, []string{importFile}); err != nil {
		t.Fatalf("runWorkspaceImport --force: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-e", nil, &detail); err != nil {
		t.Fatalf("verify import: %v", err)
	}
	if !equalStrSliceForWorkspaceTest(detail.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh] (--force did not act as --mode replace)", detail.Meta.HostCommands)
	}
}

// TestRunWorkspaceImport_AutoDetectsSlugFromBasename pins the CLI-side
// resolution of the export/import yaml shape asymmetry (docs/plans/
// workspace-db-consolidation.md PR5 brief: export bodies carry no "slug"
// key): when --slug is omitted, the target slug is derived from the
// import file's basename (extension stripped).
func TestRunWorkspaceImport_AutoDetectsSlugFromBasename(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)

	importFile := filepath.Join(t.TempDir(), "team-f.yaml")
	if err := os.WriteFile(importFile, []byte("host_commands:\n  - gh\n"), 0o644); err != nil {
		t.Fatalf("write import file: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceImportCmd
	cmd.SetOut(&out)
	if err := runWorkspaceImport(cmd, []string{importFile}); err != nil {
		t.Fatalf("runWorkspaceImport (auto-detect slug): %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-f", nil, &detail); err != nil {
		t.Fatalf("verify auto-detected slug team-f: %v", err)
	}
}

// TestRunWorkspaceImport_RejectsMultipleDocuments mirrors
// TestRunWorkspaceCreate_RejectsMultipleDocuments/
// TestRunWorkspaceEdit_RejectsMultipleDocuments for the import path: a
// multi-document import file must be rejected client-side before any daemon
// call is attempted.
func TestRunWorkspaceImport_RejectsMultipleDocuments(t *testing.T) {
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(file, []byte(multiDocWorkspaceYAML), 0o644); err != nil {
		t.Fatalf("write multi-doc yaml: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("slug", "team-a"); err != nil {
		t.Fatalf("set --slug: %v", err)
	}
	t.Setenv("BOID_SOCKET", filepath.Join(dir, "no-daemon-here.sock"))

	cmd := workspaceImportCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWorkspaceImport(cmd, []string{file})
	if err == nil {
		t.Fatal("expected an error rejecting the multi-document import file")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("expected a multi-document rejection error, got: %v", err)
	}
}

// TestRunWorkspaceImport_RejectsInvalidMode pins client-side --mode
// validation: an unrecognized --mode value must fail before any daemon call.
func TestRunWorkspaceImport_RejectsInvalidMode(t *testing.T) {
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "team-a.yaml")
	if err := os.WriteFile(file, []byte("host_commands: [gh]\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("mode", "bogus"); err != nil {
		t.Fatalf("set --mode: %v", err)
	}
	t.Setenv("BOID_SOCKET", filepath.Join(dir, "no-daemon-here.sock"))

	cmd := workspaceImportCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWorkspaceImport(cmd, []string{file})
	if err == nil {
		t.Fatal("expected an error for an invalid --mode value")
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Errorf("expected the error to mention --mode, got: %v", err)
	}
}

// TestRunWorkspaceImport_ForceAndCreateOnlyConflict pins the codex PR5
// review's minor finding: `--force` is a shorthand for `--mode replace`,
// but if the caller *also* passes `--mode create-only` explicitly, the two
// directives disagree. Silently letting --force upgrade an explicit safety
// declaration into replace is dangerous — surface the conflict as an
// error instead. --force with default --mode (unset) still translates to
// replace; --force with an explicit `--mode replace` is redundant-but-OK
// (same effect, no conflict).
func TestRunWorkspaceImport_ForceAndCreateOnlyConflict(t *testing.T) {
	resetWorkspaceExportImportFlags(t)
	defer resetWorkspaceExportImportFlags(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "team-a.yaml")
	if err := os.WriteFile(file, []byte("host_commands: [gh]\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("mode", "create-only"); err != nil {
		t.Fatalf("set --mode: %v", err)
	}
	if err := workspaceImportCmd.Flags().Set("force", "true"); err != nil {
		t.Fatalf("set --force: %v", err)
	}
	t.Setenv("BOID_SOCKET", filepath.Join(dir, "no-daemon-here.sock"))

	cmd := workspaceImportCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWorkspaceImport(cmd, []string{file})
	if err == nil {
		t.Fatal("expected an error for --force conflicting with --mode create-only")
	}
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "create-only") {
		t.Errorf("expected the error to mention both --force and create-only, got: %v", err)
	}
}

// equalStrSliceForWorkspaceTest avoids colliding with equalStrSlice /
// equalStringSliceForTest, which live in different packages/files but the
// same identifier space concerns do not apply across packages — kept
// distinct here only to avoid confusion reading this file in isolation.
func equalStrSliceForWorkspaceTest(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// MAJOR 2 (codex review round 2, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7): resolveDaemonKitsDir must hard-error on any outcome other
// than a clean 200 with a non-empty kits_dir, instead of silently falling
// back to this CLI process's own defaultKitsDir() computation.
// ---------------------------------------------------------------------------

// newFakeKitsDirServer starts a stub UNIX-socket HTTP server that responds
// to every request (regardless of path) with the given status code and raw
// body, returning a *client.Client pointed at it. Used to simulate
// GET /api/config/kits-dir failure modes (404/5xx/undecodable body) without
// needing a full testutil.TestServer — mirrors
// TestProjectMigrate_PutFailure_ShadowFileIsMergedComplete's stub server
// pattern (cmd/project_migrate_test.go).
func newFakeKitsDirServer(t *testing.T, statusCode int, body string) *client.Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "fake-kits-dir.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fakeSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = fakeSrv.Serve(ln) }()
	t.Cleanup(func() { _ = fakeSrv.Close() })
	return client.NewUnixClient(sockPath)
}

// TestResolveDaemonKitsDir_404DaemonUpgradeRequired pins MAJOR 2: a daemon
// that does not expose GET /api/config/kits-dir at all (an older, pre-PR7
// binary) must hard-error rather than silently falling back to this CLI
// process's own defaultKitsDir() — the fallback risked resolving (and
// permanently persisting, via MaterializeWorkspaceKitsForPersist) a
// workspace's kit references against the wrong directory whenever a
// same-named kit happened to exist under both locations.
func TestResolveDaemonKitsDir_404DaemonUpgradeRequired(t *testing.T) {
	c := newFakeKitsDirServer(t, http.StatusNotFound, `{"error":"not found"}`)
	_, err := resolveDaemonKitsDir(c)
	if err == nil {
		t.Fatal("expected an error for a 404 (endpoint not exposed), got nil")
	}
}

// TestResolveDaemonKitsDir_500PropagatesError pins the same hard-error
// contract for a daemon-side 5xx response.
func TestResolveDaemonKitsDir_500PropagatesError(t *testing.T) {
	c := newFakeKitsDirServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	_, err := resolveDaemonKitsDir(c)
	if err == nil {
		t.Fatal("expected an error for a 500 response, got nil")
	}
}

// TestResolveDaemonKitsDir_DecodeFailurePropagatesError pins the hard-error
// contract for a 200 response whose body does not decode as the expected
// {"kits_dir": "..."} shape.
func TestResolveDaemonKitsDir_DecodeFailurePropagatesError(t *testing.T) {
	c := newFakeKitsDirServer(t, http.StatusOK, `not json at all`)
	_, err := resolveDaemonKitsDir(c)
	if err == nil {
		t.Fatal("expected an error for an undecodable response body, got nil")
	}
}

// TestResolveDaemonKitsDir_EmptyKitsDirPropagatesError pins the last
// hard-error case discussed alongside MAJOR 2: a 200 response that decodes
// cleanly but reports an empty kits_dir must still hard-error rather than
// silently falling back to this CLI's own default — an empty value means
// this CLI does not actually know the running daemon's real kits directory
// (whatever the reason on the daemon side), and falling back risks
// resolving against the wrong location exactly like the 404/5xx cases
// above.
func TestResolveDaemonKitsDir_EmptyKitsDirPropagatesError(t *testing.T) {
	c := newFakeKitsDirServer(t, http.StatusOK, `{"kits_dir":""}`)
	_, err := resolveDaemonKitsDir(c)
	if err == nil {
		t.Fatal("expected an error for an empty kits_dir response, got nil")
	}
}

// TestResolveDaemonKitsDir_Success is the positive counterpart: a 200
// response with a non-empty kits_dir is returned as-is, with no error.
func TestResolveDaemonKitsDir_Success(t *testing.T) {
	c := newFakeKitsDirServer(t, http.StatusOK, `{"kits_dir":"/custom/kits"}`)
	got, err := resolveDaemonKitsDir(c)
	if err != nil {
		t.Fatalf("resolveDaemonKitsDir: %v", err)
	}
	if got != "/custom/kits" {
		t.Errorf("kits dir = %q, want /custom/kits", got)
	}
}

// ---------------------------------------------------------------------------
// MAJOR 3 (codex review round 2, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7): ensureWorkspaceExistsForAssign must read a local
// workspace.yaml exactly once, and a read failure other than "file does not
// exist" must hard-error rather than silently falling through to the
// kits-oblivious first-read's content.
// ---------------------------------------------------------------------------

// TestRunWorkspaceAssign_AutoCreate_ReadFailure_HardError pins MAJOR 3: a
// local workspace.yaml read failure other than "file does not exist" (here,
// a directory sitting where a file was expected — a portable way to force a
// non-ENOENT os.ReadFile error without relying on permission bits, which
// root ignores) must hard-error the auto-create step, not silently produce
// a confusing downstream 404 the way an unrelated failure being swallowed
// used to.
func TestRunWorkspaceAssign_AutoCreate_ReadFailure_HardError(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		t.Fatalf("DefaultWorkspaceDir: %v", err)
	}
	// Force a read failure that is NOT os.ErrNotExist: a directory at the
	// exact path a workspace.yaml file would live makes os.ReadFile fail
	// with "is a directory" instead of ENOENT.
	badPath := filepath.Join(wsDir, "unreadable-ws.yaml")
	if err := os.MkdirAll(badPath, 0o755); err != nil {
		t.Fatalf("mkdir (simulating an unreadable workspace.yaml): %v", err)
	}

	dir := writeImportTestProject(t, "assign-proj-unreadable", "Assign Proj Unreadable")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	err = runWorkspaceAssign(cmd, []string{project.ID, "unreadable-ws"})
	if err == nil {
		t.Fatal("expected the local workspace.yaml read failure to surface as a hard error, got nil")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("error must report the actual read failure, not a generic 'not found': %v", err)
	}
}

// TestRunWorkspaceAssign_AutoCreate_SnapshotAvoidsTOCTOU pins MAJOR 3:
// ensureWorkspaceExistsForAssign must read a local workspace.yaml exactly
// once, deriving both its meta and its legacy `kits:` reference list from
// that single byte snapshot — mirroring readWorkspaceYAMLSnapshot's
// (workspace_migration.go) MAJOR 5 fix on the server side. Before this fix,
// this function read the file via orchestrator.NewWorkspaceStore("").Load
// (one os.ReadFile) and then, later, a second independent os.ReadFile of
// the very same path to extract the kits: list and strictly validate the
// remainder — a second read this test proves no longer happens.
func TestRunWorkspaceAssign_AutoCreate_SnapshotAvoidsTOCTOU(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		t.Fatalf("DefaultWorkspaceDir: %v", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "toctou-ws.yaml"), []byte("host_commands:\n  - gh\n"), 0o644); err != nil {
		t.Fatalf("write workspace.yaml: %v", err)
	}

	readCount := 0
	orig := localWorkspaceYAMLReadFile
	localWorkspaceYAMLReadFile = func(path string) ([]byte, error) {
		readCount++
		return orig(path)
	}
	t.Cleanup(func() { localWorkspaceYAMLReadFile = orig })

	dir := writeImportTestProject(t, "assign-proj-toctou", "Assign Proj TOCTOU")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	cmd := workspaceAssignCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runWorkspaceAssign(cmd, []string{project.ID, "toctou-ws"}); err != nil {
		t.Fatalf("runWorkspaceAssign: %v", err)
	}

	if readCount != 1 {
		t.Errorf("localWorkspaceYAMLReadFile called %d times, want exactly 1 (a second read reintroduces the TOCTOU this fix closes)", readCount)
	}
}

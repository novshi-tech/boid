package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/testutil"
)

// TestRunConfigGet_FullDump_FreshInstallIsEmpty pins `boid config get`'s
// sparse contract (GET /api/config returns config.yaml exactly as it sits
// on disk, not a defaults-expanded view — see internal/server/
// config_edit.go's ConfigYAML doc comment): a fresh daemon with nothing
// explicitly configured yet dumps an empty document.
func TestRunConfigGet_FullDump_FreshInstallIsEmpty(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := configGetCmd
	cmd.SetOut(&out)
	if err := runConfigGet(cmd, nil); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	if got := out.String(); strings.TrimSpace(got) != "" {
		t.Errorf("expected empty dump on a fresh install, got:\n%s", got)
	}
}

func TestRunConfigGet_FullDump_ReflectsWhatWasSet(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	setCmd := configSetCmd
	setCmd.SetOut(&bytes.Buffer{})
	if err := runConfigSet(setCmd, []string{"sandbox.backend", "container"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	var out bytes.Buffer
	cmd := configGetCmd
	cmd.SetOut(&out)
	if err := runConfigGet(cmd, nil); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "backend: container") {
		t.Errorf("expected the just-set sandbox.backend in full dump, got:\n%s", got)
	}
}

func TestRunConfigGet_SingleKey(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	setCmd := configSetCmd
	setCmd.SetOut(&bytes.Buffer{})
	if err := runConfigSet(setCmd, []string{"sandbox.backend", "container"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	var out bytes.Buffer
	cmd := configGetCmd
	cmd.SetOut(&out)
	if err := runConfigGet(cmd, []string{"sandbox.backend"}); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "container" {
		t.Errorf("get sandbox.backend = %q, want container", got)
	}
}

// TestRunConfigGet_SingleKey_NotExplicitlySet pins the flip side: a schema-
// known key that has never been explicitly written (using its built-in
// default at runtime) is "not found" from get/unset's point of view — see
// internal/server/config_edit.go's ConfigYAML doc comment for why this is
// the deliberate, sparse-round-trip design rather than a bug.
func TestRunConfigGet_SingleKey_NotExplicitlySet(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	cmd := configGetCmd
	cmd.SetOut(&bytes.Buffer{})
	err := runConfigGet(cmd, []string{"sandbox.backend"})
	if err == nil {
		t.Fatal("expected 'key not found' for a never-explicitly-set key")
	}
	if !strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected 'key not found', got: %v", err)
	}
}

func TestRunConfigGet_UnknownKey(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := configGetCmd
	cmd.SetOut(&out)
	err := runConfigGet(cmd, []string{"sandbox.alowed_domains"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("expected suggestion, got: %v", err)
	}
}

func TestRunConfigSet_Scalar_ThenGet(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var setOut bytes.Buffer
	setCmd := configSetCmd
	setCmd.SetOut(&setOut)
	if err := runConfigSet(setCmd, []string{"web.public_url", "https://boid.example.com"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}
	if !strings.Contains(setOut.String(), "config applied") {
		t.Errorf("expected confirmation, got: %s", setOut.String())
	}

	var getOut bytes.Buffer
	getCmd := configGetCmd
	getCmd.SetOut(&getOut)
	if err := runConfigGet(getCmd, []string{"web.public_url"}); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	if got := strings.TrimSpace(getOut.String()); got != "https://boid.example.com" {
		t.Errorf("get web.public_url = %q, want https://boid.example.com", got)
	}
}

func TestRunConfigSet_Array_WholesaleReplace(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	setCmd := configSetCmd
	setCmd.SetOut(&bytes.Buffer{})
	if err := runConfigSet(setCmd, []string{"sandbox.allowed_domains", ".a.com"}); err != nil {
		t.Fatalf("runConfigSet 1: %v", err)
	}
	if err := runConfigSet(setCmd, []string{"sandbox.allowed_domains", ".b.com", ".c.com"}); err != nil {
		t.Fatalf("runConfigSet 2: %v", err)
	}

	var getOut bytes.Buffer
	getCmd := configGetCmd
	getCmd.SetOut(&getOut)
	if err := runConfigGet(getCmd, []string{"sandbox.allowed_domains"}); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	got := getOut.String()
	if strings.Contains(got, ".a.com") {
		t.Errorf("expected wholesale replace (no .a.com), got:\n%s", got)
	}
	if !strings.Contains(got, ".b.com") || !strings.Contains(got, ".c.com") {
		t.Errorf("expected .b.com and .c.com, got:\n%s", got)
	}

	// BLOCKER 2 (codex review round 1): the effective list is the built-in
	// floor UNION the just-set user entries (.a.com must be gone, but the
	// built-in floor is never dropped by a set) — not an exact 2-entry
	// list, which was the pre-fix (buggy) contract this assertion used to
	// pin.
	if got := ts.Server.AllowedDomains(); !cmdTestContainsAll(got, ".b.com", ".c.com") {
		t.Errorf("AllowedDomains() (hot-reload) = %v, want it to contain .b.com and .c.com", got)
	} else if hasAny(got, ".a.com") {
		t.Errorf("AllowedDomains() (hot-reload) = %v, .a.com should have been replaced away", got)
	}
}

// cmdTestContainsAll reports whether every want is present in got.
func cmdTestContainsAll(got []string, wants ...string) bool {
	set := make(map[string]struct{}, len(got))
	for _, g := range got {
		set[g] = struct{}{}
	}
	for _, w := range wants {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func hasAny(got []string, wants ...string) bool {
	for _, g := range got {
		for _, w := range wants {
			if g == w {
				return true
			}
		}
	}
	return false
}

func TestRunConfigSet_UnknownKey_NoDaemonCall(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	setCmd := configSetCmd
	setCmd.SetOut(&bytes.Buffer{})
	err := runConfigSet(setCmd, []string{"sandbox.alowed_domains", "x"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestRunConfigUnset_ExistingKey(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	setCmd := configSetCmd
	setCmd.SetOut(&bytes.Buffer{})
	if err := runConfigSet(setCmd, []string{"web.public_url", "https://x.example.com"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	unsetCmd := configUnsetCmd
	unsetCmd.SetOut(&bytes.Buffer{})
	if err := runConfigUnset(unsetCmd, []string{"web.public_url"}); err != nil {
		t.Fatalf("runConfigUnset: %v", err)
	}

	getCmd := configGetCmd
	var getOut bytes.Buffer
	getCmd.SetOut(&getOut)
	if err := runConfigGet(getCmd, []string{"web.public_url"}); err == nil {
		t.Errorf("expected 'key not found' after unset, got value: %s", getOut.String())
	}
}

func TestRunConfigUnset_NonExistentKey(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	unsetCmd := configUnsetCmd
	unsetCmd.SetOut(&bytes.Buffer{})
	err := runConfigUnset(unsetCmd, []string{"web.public_url"})
	if err == nil {
		t.Fatal("expected error: key not found")
	}
	if !strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected 'key not found', got: %v", err)
	}
}

func TestRunConfigUnset_WholeForgeEntry(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	setCmd := configSetCmd
	setCmd.SetOut(&bytes.Buffer{})
	if err := runConfigSet(setCmd, []string{"gateway.forges.github.secret_key", "my-pat"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	var unsetOut bytes.Buffer
	unsetCmd := configUnsetCmd
	unsetCmd.SetOut(&unsetOut)
	if err := runConfigUnset(unsetCmd, []string{"gateway.forges.github"}); err != nil {
		t.Fatalf("runConfigUnset: %v", err)
	}
	if !strings.Contains(unsetOut.String(), "requires daemon restart") {
		t.Errorf("expected restart-required warning, got: %s", unsetOut.String())
	}
}

func TestRunConfigApply_ValidFile(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	dir := t.TempDir()
	f := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(f, []byte("sandbox:\n  allowed_domains:\n    - .apply-test.com\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	configApplyFile = f
	defer func() { configApplyFile = "" }()

	var out bytes.Buffer
	applyCmd := configApplyCmd
	applyCmd.SetOut(&out)
	if err := runConfigApply(applyCmd, nil); err != nil {
		t.Fatalf("runConfigApply: %v", err)
	}
	if !strings.Contains(out.String(), "config applied") {
		t.Errorf("expected confirmation, got: %s", out.String())
	}

	got := ts.Server.AllowedDomains()
	if !cmdTestContainsAll(got, ".apply-test.com") {
		t.Errorf("AllowedDomains() = %v, want it to contain .apply-test.com", got)
	}
}

// TestRunConfigApply_InvalidFile_NoDaemonCall points BOID_SOCKET at a
// nonexistent socket path, proving the client-side ValidateYAML pre-check
// (cmd/config.go's runConfigApply, mirroring runWorkspaceEdit's MINOR 1
// precedent) rejects a bad file before ever attempting to dial the daemon —
// if it dialed first, this would fail with a connection error instead of
// the validation error this test asserts on.
func TestRunConfigApply_InvalidFile_NoDaemonCall(t *testing.T) {
	t.Setenv("BOID_SOCKET", filepath.Join(t.TempDir(), "nonexistent.sock"))

	dir := t.TempDir()
	f := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(f, []byte("default_harness: claude-code\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	configApplyFile = f
	defer func() { configApplyFile = "" }()

	applyCmd := configApplyCmd
	applyCmd.SetOut(&bytes.Buffer{})
	err := runConfigApply(applyCmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "default_harness") {
		t.Errorf("expected error to name default_harness, got: %v", err)
	}
}

func TestRunConfigApply_MissingFileFlag(t *testing.T) {
	configApplyFile = ""
	applyCmd := configApplyCmd
	applyCmd.SetOut(&bytes.Buffer{})
	if err := runConfigApply(applyCmd, nil); err == nil {
		t.Fatal("expected error when -f is missing")
	}
}

// TestPostConfigApply_ConflictingRevision_RejectedWithHelpfulMessage pins
// BLOCKER 1 (codex review round 1): posting a deliberately stale If-Match
// (simulating "the config changed since this file was validated") is
// rejected (412/428), not silently applied — and the error names --force /
// re-running apply as the remediation, exactly runConfigApply's own
// contract, tested directly against postConfigApply so the scenario is
// deterministic (no need to win a race against runConfigApply's own
// internal fetch-then-POST window to produce a genuinely stale revision).
func TestPostConfigApply_ConflictingRevision_RejectedWithHelpfulMessage(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	c := client.NewUnixClient(ts.Server.SocketPath())

	_, staleRev, err := fetchConfigYAML(c)
	if err != nil {
		t.Fatalf("fetchConfigYAML: %v", err)
	}
	// Someone else applies, advancing the daemon's revision past staleRev.
	if _, err := ts.Server.ApplyConfigYAML([]byte("web:\n  public_url: https://other.example.com\n"), "", true); err != nil {
		t.Fatalf("seed ApplyConfigYAML: %v", err)
	}

	var out bytes.Buffer
	applyCmd := configApplyCmd
	applyCmd.SetOut(&out)
	err = postConfigApply(applyCmd, c, []byte("sandbox:\n  allowed_domains:\n    - .conflict-test.com\n"), staleRev, false, "config.yaml")
	if err == nil {
		t.Fatal("expected a conflict error for a stale revision")
	}
	if !strings.Contains(err.Error(), "boid config apply -f") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("expected the error to name apply -f / --force as remediation, got: %v", err)
	}

	// The conflicting apply must NOT have been persisted.
	data, _, err := ts.Server.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if strings.Contains(string(data), "conflict-test.com") {
		t.Errorf("conflicting apply was persisted despite the revision mismatch:\n%s", data)
	}
}

// TestPostConfigApply_Force_BypassesRevisionCheck pins --force's contract:
// even a stale/empty If-Match is accepted when force is true.
func TestPostConfigApply_Force_BypassesRevisionCheck(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	c := client.NewUnixClient(ts.Server.SocketPath())

	var out bytes.Buffer
	applyCmd := configApplyCmd
	applyCmd.SetOut(&out)
	if err := postConfigApply(applyCmd, c, []byte("sandbox:\n  allowed_domains:\n    - .force-test.com\n"), "", true, "config.yaml"); err != nil {
		t.Fatalf("postConfigApply --force: %v", err)
	}
	if !strings.Contains(out.String(), "config applied") {
		t.Errorf("expected confirmation, got: %s", out.String())
	}
	if got := ts.Server.AllowedDomains(); !cmdTestContainsAll(got, ".force-test.com") {
		t.Errorf("AllowedDomains() = %v, want it to contain .force-test.com", got)
	}
}

func TestRunConfigEdit_AppliesChange(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	// Stand-in "editor": a script overwriting its argument ($1, the temp
	// file path cmd/config.go appends) with new content — no interactive
	// terminal needed. exec.Command splits $EDITOR on whitespace and
	// appends the temp file path as the final argument (cmd/config.go's
	// runConfigEdit), so a plain script path as EDITOR works unmodified.
	script := filepath.Join(t.TempDir(), "fake-editor.sh")
	scriptBody := "#!/bin/sh\nprintf 'sandbox:\\n  allowed_domains: [\".edit-test.com\"]\\n' > \"$1\"\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake editor script: %v", err)
	}
	t.Setenv("EDITOR", script)

	var out bytes.Buffer
	editCmd := configEditCmd
	editCmd.SetOut(&out)
	if err := runConfigEdit(editCmd, nil); err != nil {
		t.Fatalf("runConfigEdit: %v", err)
	}
	if !strings.Contains(out.String(), "config applied") {
		t.Errorf("expected confirmation, got: %s", out.String())
	}

	got := ts.Server.AllowedDomains()
	if !cmdTestContainsAll(got, ".edit-test.com") {
		t.Errorf("AllowedDomains() = %v, want it to contain .edit-test.com", got)
	}
}

func TestRunConfigEdit_NoChanges(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("EDITOR", "true") // touches nothing

	var out bytes.Buffer
	editCmd := configEditCmd
	editCmd.SetOut(&out)
	if err := runConfigEdit(editCmd, nil); err != nil {
		t.Fatalf("runConfigEdit: %v", err)
	}
	if !strings.Contains(out.String(), "no changes") {
		t.Errorf("expected 'no changes', got: %s", out.String())
	}
}

func TestRunConfigEdit_ValidationFailure_KeepsTempFile(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	// Overwrite the temp file with something that fails schema validation.
	script := filepath.Join(t.TempDir(), "bad-editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'default_harness: claude-code' > \"$1\"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("EDITOR", script)

	editCmd := configEditCmd
	editCmd.SetOut(&bytes.Buffer{})
	err := runConfigEdit(editCmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "boid config apply -f") {
		t.Errorf("expected error to point at `boid config apply -f <path>`, got: %v", err)
	}
	// Extract the kept temp path from the error and confirm it still exists.
	msg := err.Error()
	idx := strings.Index(msg, "kept at ")
	if idx == -1 {
		t.Fatalf("could not find kept-at path in error: %v", err)
	}
	rest := msg[idx+len("kept at "):]
	end := strings.IndexAny(rest, " —")
	if end == -1 {
		end = len(rest)
	}
	path := strings.TrimSpace(rest[:end])
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("expected temp file %q to still exist: %v", path, statErr)
	}
}

func TestRunConfigEdit_EmptyEditorEnv_DefaultsToVi(t *testing.T) {
	// Not actually exercised interactively — just confirms an unset
	// $EDITOR does not itself error out before exec attempts "vi" (which
	// may or may not exist in the test environment; either outcome is
	// acceptable here, this only guards against a nil-slice/empty-Fields
	// panic in the fallback path).
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	os.Unsetenv("EDITOR")

	editCmd := configEditCmd
	editCmd.SetOut(&bytes.Buffer{})
	// Either "vi" is on PATH (unlikely in CI, would hang without a TTY —
	// so we don't assert success) or exec fails with "executable file not
	// found" — both are non-panicking outcomes, which is all this test
	// pins. Run in a way that can't hang: PATH is cleared so exec.Command
	// fails fast with "not found" instead of launching a real vi.
	t.Setenv("PATH", "")
	if err := runConfigEdit(editCmd, nil); err == nil {
		t.Fatal("expected an error (vi not found with empty PATH)")
	}
}

package sandbox_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestResolveShimCommandName_FallsBackToBasenameWhenEnvUnset covers the
// common (non-alias) case: no BOID_HOST_COMMAND_NAMES is set, so the shim
// falls back to CommandFromArgv0 — which already equals the declared short
// name whenever host_commands.<name>.path is unset or its basename matches
// name (the overwhelming majority of host_commands entries, e.g. "gh").
func TestResolveShimCommandName_FallsBackToBasenameWhenEnvUnset(t *testing.T) {
	t.Setenv(sandbox.HostCommandNamesEnv, "")

	got := sandbox.ResolveShimCommandName("/usr/bin/gh")
	if got != "gh" {
		t.Errorf("ResolveShimCommandName() = %q, want %q", got, "gh")
	}
}

// TestResolveShimCommandName_AliasedPathResolvesToDeclaredName is the codex
// review Minor fix this PR closes (docs/plans/phase5-shim-and-task-context.md
// 5a-2): a host_commands.<name>.path alias (e.g. run-e2e -> e2e/run.sh) means
// the shim's own bind-mount basename ("run.sh") never equals the declared
// short name ("run-e2e"). BOID_HOST_COMMAND_NAMES maps this shim's own
// bind-mount path (what shimBinaryPath/os.Executable() resolves to) to the
// declared name, so the resolution must prefer that map entry over the
// argv0 basename.
func TestResolveShimCommandName_AliasedPathResolvesToDeclaredName(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}

	names := map[string]string{exe: "run-e2e"}
	raw, err := json.Marshal(names)
	if err != nil {
		t.Fatalf("marshal names: %v", err)
	}
	t.Setenv(sandbox.HostCommandNamesEnv, string(raw))

	// argv0's basename ("run.sh") deliberately does not match the declared
	// name ("run-e2e") — that mismatch is exactly what the env map exists to
	// bridge.
	got := sandbox.ResolveShimCommandName("run.sh")
	if got != "run-e2e" {
		t.Errorf("ResolveShimCommandName() = %q, want %q", got, "run-e2e")
	}
}

// TestResolveShimCommandName_ExeNotInMapFallsBackToBasename covers a map
// that is present but has no entry for this shim's own exe path (e.g. a
// stale/foreign map, or a shim binary running outside any declared
// host_commands mount). It must fall back rather than error out.
func TestResolveShimCommandName_ExeNotInMapFallsBackToBasename(t *testing.T) {
	t.Setenv(sandbox.HostCommandNamesEnv, `{"/some/other/path":"other-name"}`)

	got := sandbox.ResolveShimCommandName("/usr/bin/gh")
	if got != "gh" {
		t.Errorf("ResolveShimCommandName() = %q, want %q", got, "gh")
	}
}

// TestResolveShimCommandName_MalformedJSONFallsBackToBasename mirrors
// EarlyReject's malformed-JSON handling: the env var is a best-effort UX
// fast path, never a hard dependency, so garbage input must not crash or
// block — just fall through to the basename.
func TestResolveShimCommandName_MalformedJSONFallsBackToBasename(t *testing.T) {
	t.Setenv(sandbox.HostCommandNamesEnv, `{"not":`)

	got := sandbox.ResolveShimCommandName("/usr/bin/gh")
	if got != "gh" {
		t.Errorf("ResolveShimCommandName() = %q, want %q", got, "gh")
	}
}

// TestResolveShimCommandName_EmptyMapValueFallsBackToBasename guards against
// a map entry present but empty (e.g. produced by a future dispatcher bug) —
// an empty resolved name must never be sent to the broker as a command name.
func TestResolveShimCommandName_EmptyMapValueFallsBackToBasename(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	names := map[string]string{exe: ""}
	raw, err := json.Marshal(names)
	if err != nil {
		t.Fatalf("marshal names: %v", err)
	}
	t.Setenv(sandbox.HostCommandNamesEnv, string(raw))

	got := sandbox.ResolveShimCommandName("gh")
	if got != "gh" {
		t.Errorf("ResolveShimCommandName() = %q, want %q", got, "gh")
	}
}

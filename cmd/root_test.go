package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/profiles"
	"github.com/spf13/cobra"
)

// newProfileTestCmd builds a standalone leaf *cobra.Command carrying the
// same --profile flag rootCmd registers, without touching the shared
// package-level rootCmd singleton — mirrors output_test.go's newTestRoot,
// which exists for the identical reason (avoid cross-test global-flag-state
// pollution within this package's single test binary; task_inspect_test.go's
// setOutputFormat shows the alternative, mutate-and-restore-via-t.Cleanup
// approach this file deliberately avoids needing).
func newProfileTestCmd(t *testing.T, profile string, annotations map[string]string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "leaf", Annotations: annotations}
	cmd.Flags().String(profiles.ProfileFlagName, "", "")
	if profile != "" {
		if err := cmd.Flags().Set(profiles.ProfileFlagName, profile); err != nil {
			t.Fatalf("set --profile: %v", err)
		}
	}
	return cmd
}

// writeRootTestConfigYAML isolates $XDG_CONFIG_HOME to a fresh temp dir and
// writes content as ~/.config/boid/config.yaml under it.
func writeRootTestConfigYAML(t *testing.T, content string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if content == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(configDir, "boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "boid", "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func writeRootTestTokenFile(t *testing.T, profileName, content string) {
	t.Helper()
	dir, err := profiles.TokensDir()
	if err != nil {
		t.Fatalf("TokensDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir tokens dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, profileName+".json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

// --- resolveClient (TDD step 8's non-cobra-execution half: unit-level) ---

func TestResolveClient_NoProfile_ReturnsUnixClient(t *testing.T) {
	writeRootTestConfigYAML(t, "") // no config.yaml at all
	cmd := newProfileTestCmd(t, "", nil)

	c, err := resolveClient(cmd)
	if err != nil {
		t.Fatalf("resolveClient: %v", err)
	}
	if !c.IsUnix() {
		t.Error("expected the 現行互換 unix fallback client")
	}
}

func TestResolveClient_UnknownProfileFlag_Error(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles: {}\n")
	cmd := newProfileTestCmd(t, "ghost", nil)

	_, err := resolveClient(cmd)
	if err == nil {
		t.Fatal("expected an error for an undefined --profile value")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the profile, got %q", err.Error())
	}
}

func TestResolveClient_HTTPSProfile_ReturnsNonUnixClient(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles:\n  work:\n    url: https://work.example.com\n")
	writeRootTestTokenFile(t, "work", `{"device_id":"d","token":"tk_x","url":"https://work.example.com"}`)
	cmd := newProfileTestCmd(t, "work", nil)

	c, err := resolveClient(cmd)
	if err != nil {
		t.Fatalf("resolveClient: %v", err)
	}
	if c.IsUnix() {
		t.Error("expected an https-scheme client for an https profile")
	}
}

// --- rootCmd.PersistentPreRunE, invoked through cobra's own field (TDD
// step 8: "root PersistentPreRunE の client 注入 test (cobra 経由)") ---

func TestPersistentPreRunE_InjectsResolvedClientIntoContext(t *testing.T) {
	writeRootTestConfigYAML(t, "")
	t.Setenv("BOID_SOCKET", filepath.Join(t.TempDir(), "pinned-for-test.sock"))
	cmd := newProfileTestCmd(t, "", map[string]string{annotationSkipAutostart: "skip"})

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	c := client.FromContext(cmd.Context())
	if c == nil {
		t.Fatal("expected a client to have been injected into cmd's context")
	}
	if !c.IsUnix() {
		t.Error("expected the default unix client to have been injected")
	}
}

// TestPersistentPreRunE_CompletionScriptGen_BypassesProfileResolve pins
// that `boid completion bash|zsh|fish|powershell` (Cobra's static-script
// generator) must NOT fail on a broken profile file — the whole point of
// running that command is often to re-install completion AFTER something
// like a token file broke.
//
// The real Cobra tree here is `root → completion → bash`, so the leaf
// (`bash`) has `completion` as a walkable ancestor; isCompletionScriptGen
// walks the parent chain looking for that exact name.
func TestPersistentPreRunE_CompletionScriptGen_BypassesProfileResolve(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles: {}\n")

	completionCmd := &cobra.Command{Use: "completion"}
	leaf := &cobra.Command{Use: "bash"}
	leaf.Flags().String(profiles.ProfileFlagName, "", "")
	// A profile name that would normally cause "profile \"ghost\" is not defined".
	if err := leaf.Flags().Set(profiles.ProfileFlagName, "ghost"); err != nil {
		t.Fatalf("set --profile: %v", err)
	}
	completionCmd.AddCommand(leaf)

	if err := rootCmd.PersistentPreRunE(leaf, nil); err != nil {
		t.Errorf("completion script gen must not fail on a broken profile: %v", err)
	}
}

// TestPersistentPreRunE_CompletionQuery_BrokenProfile_SilentDegrade pins
// that a Cobra TAB-completion invocation with a broken profile silently
// degrades rather than surfacing an error to the user's shell.
//
// Cobra tree shape: the real `__complete` hidden command is a direct
// child of root — the target command whose completion is being computed
// is passed in `args`, NOT constructed as a child of `__complete` (this
// was the codex PR1 round-3 correction to an earlier fixture that
// modelled `__complete → task` as parent/child). Consequently the
// PersistentPreRunE guard fires on `cmd.Name() == "__complete"` on the
// entrypoint command itself, which is what the production path sees at
// runtime.
func TestPersistentPreRunE_CompletionQuery_BrokenProfile_SilentDegrade(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles: {}\n")

	completeCmd := &cobra.Command{Use: "__complete"}
	completeCmd.Flags().String(profiles.ProfileFlagName, "", "")
	if err := completeCmd.Flags().Set(profiles.ProfileFlagName, "ghost"); err != nil {
		t.Fatalf("set --profile: %v", err)
	}

	if err := rootCmd.PersistentPreRunE(completeCmd, []string{"task"}); err != nil {
		t.Errorf("__complete must not surface profile errors: %v", err)
	}
}

// TestPersistentPreRunE_CompletionQuery_LeavesContextClientUninjected
// pins the docs/plans/cli-remote-connection.md PR1 round-3 fix: when
// profile resolution fails on a completion query, root's
// PersistentPreRunE swallows the error but must NOT inject a client (it
// would silently be the default UNIX client, causing completeProjectRefs
// to query the wrong daemon). Downstream callbacks use
// client.FromContextOrNil (not FromContext) to detect this and return
// no candidates.
func TestPersistentPreRunE_CompletionQuery_LeavesContextClientUninjected(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles: {}\n")

	completeCmd := &cobra.Command{Use: "__complete"}
	completeCmd.Flags().String(profiles.ProfileFlagName, "", "")
	if err := completeCmd.Flags().Set(profiles.ProfileFlagName, "ghost"); err != nil {
		t.Fatalf("set --profile: %v", err)
	}

	if err := rootCmd.PersistentPreRunE(completeCmd, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	if c := client.FromContextOrNil(completeCmd.Context()); c != nil {
		t.Errorf("expected no client injected after silent completion degrade; got %+v", c)
	}
}

func TestPersistentPreRunE_UnknownProfile_ReturnsError(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles: {}\n")
	cmd := newProfileTestCmd(t, "ghost", nil)

	err := rootCmd.PersistentPreRunE(cmd, nil)
	if err == nil {
		t.Fatal("expected an error for an undefined --profile value")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the profile, got %q", err.Error())
	}
}

// TestPersistentPreRunE_NeutralScope_SwallowsUnknownProfileError pins
// docs/plans/cli-remote-connection.md PR2: a scope=neutral command (login/
// logout) must not be blocked by the exact same "profile ... is not
// defined" failure TestPersistentPreRunE_UnknownProfile_ReturnsError above
// pins as a hard error for an ordinary (non-neutral) command — this is the
// realistic `boid login <url> --profile <brand-new-name>` shape, where the
// named profile is by definition absent from config.yaml (it doesn't exist
// until login finishes writing it).
func TestPersistentPreRunE_NeutralScope_SwallowsUnknownProfileError(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles: {}\n")
	cmd := newProfileTestCmd(t, "ghost", map[string]string{scopeAnnotationKey: scopeNeutral})

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("a neutral-scope command must not fail on an undefined --profile value: %v", err)
	}
	if c := client.FromContextOrNil(cmd.Context()); c != nil {
		t.Errorf("expected no client injected for a neutral-scope command after a swallowed resolution error; got %+v", c)
	}
}

// TestPersistentPreRunE_NeutralScope_BrokenDefaultProfileToken_Swallowed
// pins the `boid logout <profile>` self-locking scenario: default_profile
// points at the very profile whose token is corrupt, so an ordinary
// command would hard-fail in profiles.Resolve before ever reaching
// runLogout's own cleanup logic. Neutral scope must swallow this too.
func TestPersistentPreRunE_NeutralScope_BrokenDefaultProfileToken_Swallowed(t *testing.T) {
	writeRootTestConfigYAML(t, "default_profile: work\nprofiles:\n  work:\n    url: https://work.example.com\n")
	writeRootTestTokenFile(t, "work", `{"device_id":"d","token":"","url":"https://work.example.com"}`) // empty token -> LoadToken hard error
	cmd := newProfileTestCmd(t, "", map[string]string{scopeAnnotationKey: scopeNeutral})

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("a neutral-scope command must not fail when default_profile's token is broken: %v", err)
	}
}

// TestPersistentPreRunE_UnixProfile_RunsAutostartCheck pins the "before"
// half of decision 6 (docs/plans/cli-remote-connection.md: daemon autostart
// only applies to a unix-scheme profile): with BOID_NO_AUTOSTART=1 and no
// daemon listening on the pinned socket, a unix-profile invocation (no
// boid.autostart=skip annotation) must still reach — and fail through —
// client.EnsureRunning's own no-autostart error path, proving the
// autostart check actually ran.
func TestPersistentPreRunE_UnixProfile_RunsAutostartCheck(t *testing.T) {
	writeRootTestConfigYAML(t, "")
	t.Setenv("BOID_SOCKET", filepath.Join(t.TempDir(), "no-daemon-here.sock"))
	t.Setenv(client.NoAutostartEnv, "1")
	cmd := newProfileTestCmd(t, "", nil) // no skip annotation

	err := rootCmd.PersistentPreRunE(cmd, nil)
	if err == nil {
		t.Fatal("expected an error: BOID_NO_AUTOSTART=1 and no daemon listening on a unix profile")
	}
	if !strings.Contains(err.Error(), "boid server is not running") {
		t.Errorf("expected EnsureRunning's no-autostart error, got %v", err)
	}
}

// TestPersistentPreRunE_HTTPSProfile_SkipsAutostartCheck pins the "after"
// half of decision 6: an https-scheme profile must never even ask
// client.EnsureRunning — BOID_NO_AUTOSTART is irrelevant here precisely
// because the check is skipped outright, not merely made to no-op.
func TestPersistentPreRunE_HTTPSProfile_SkipsAutostartCheck(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles:\n  work:\n    url: https://work.example.com\n")
	writeRootTestTokenFile(t, "work", `{"device_id":"d","token":"tk_x","url":"https://work.example.com"}`)
	t.Setenv(client.NoAutostartEnv, "1")
	cmd := newProfileTestCmd(t, "work", nil) // no skip annotation

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("an https profile must not trigger the autostart check at all: %v", err)
	}
	c := client.FromContext(cmd.Context())
	if c.IsUnix() {
		t.Error("expected an https-scheme client to have been injected")
	}
}

// --- scope=local rejection UX (docs/plans/cli-remote-connection.md
// decision 6, PR4) ---

// TestPersistentPreRunE_ScopeLocal_HTTPSProfile_HardError pins the core of
// decision 6: a scope=local command (e.g. `start`) resolved against a
// non-unix (https) profile must hard-fail rather than silently operate
// against the wrong host. The error must name the command's full path, the
// profile's URL, and the profile's name so the user can act on it.
func TestPersistentPreRunE_ScopeLocal_HTTPSProfile_HardError(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles:\n  work:\n    url: https://work.example.com\n")
	writeRootTestTokenFile(t, "work", `{"device_id":"d","token":"tk_x","url":"https://work.example.com"}`)
	cmd := newProfileTestCmd(t, "work", map[string]string{scopeAnnotationKey: scopeLocal})
	cmd.Use = "start" // CommandPath() reflects this in the error message

	err := rootCmd.PersistentPreRunE(cmd, nil)
	if err == nil {
		t.Fatal("expected a hard error for a scope=local command against an https profile")
	}
	// Pin BOTH the runtime substitutions (command name / URL / profile
	// name) AND the spec decision-6 phrase-book (口語調 "だよ" / "してね"
	// wording, plus the --profile hint) — codex PR4 review round 1 minor
	// 3 flagged that pinning "start" alone was too loose to catch a
	// wording drift.
	for _, want := range []string{
		"start",
		"https://work.example.com",
		"work",
		"ローカル専用コマンドだよ",
		"--profile <local-profile>",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message should contain %q, got %q", want, err.Error())
		}
	}
	if c := client.FromContextOrNil(cmd.Context()); c != nil {
		t.Errorf("expected no client injected after a scope=local rejection; got %+v", c)
	}
}

// TestPersistentPreRunE_ScopeLocal_HTTPSProfile_MissingToken_StillLocalReject
// is the codex PR4 review round 1 MAJOR 1 regression: an https profile
// whose token file is missing or corrupt would previously cause
// Resolve() to fail on token load BEFORE the scope=local check ran, so
// the caller saw "no device token for profile ... run 'boid login'
// first" instead of the actual reason (the command is local-only and
// this profile is not local). Two-phase resolution
// (profiles.ResolveWithoutToken then Resolve) fixes it: the scope=local
// message wins, and the caller does not waste time re-logging in on a
// command that will never accept the token anyway.
func TestPersistentPreRunE_ScopeLocal_HTTPSProfile_MissingToken_StillLocalReject(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles:\n  work:\n    url: https://work.example.com\n")
	// Deliberately NO writeRootTestTokenFile — Resolve() would fail here.
	cmd := newProfileTestCmd(t, "work", map[string]string{scopeAnnotationKey: scopeLocal})
	cmd.Use = "start"

	err := rootCmd.PersistentPreRunE(cmd, nil)
	if err == nil {
		t.Fatal("expected a hard error")
	}
	if !strings.Contains(err.Error(), "ローカル専用コマンドだよ") {
		t.Errorf("error should be the scope=local rejection, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "boid login") {
		t.Errorf("scope=local reject must PRE-EMPT the missing-token message, got %q", err.Error())
	}
}

// TestPersistentPreRunE_ScopeLocal_UnixProfile_Allowed pins the "before"
// half of decision 6: a scope=local command against the default (unix
// fallback) profile is unaffected — this is the overwhelmingly common case
// pre-Phase-3 and must keep working exactly as before.
func TestPersistentPreRunE_ScopeLocal_UnixProfile_Allowed(t *testing.T) {
	writeRootTestConfigYAML(t, "")
	t.Setenv("BOID_SOCKET", filepath.Join(t.TempDir(), "pinned-for-test.sock"))
	cmd := newProfileTestCmd(t, "", map[string]string{
		scopeAnnotationKey:      scopeLocal,
		annotationSkipAutostart: "skip",
	})

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("a scope=local command against a unix profile must not be rejected: %v", err)
	}
	c := client.FromContext(cmd.Context())
	if c == nil {
		t.Fatal("expected a client to have been injected")
	}
	if !c.IsUnix() {
		t.Error("expected the default unix client to have been injected")
	}
}

// TestPersistentPreRunE_ScopeRemote_HTTPSProfile_Allowed pins that
// scope=remote commands are exactly the ones decision 6's rejection does
// NOT apply to — an https profile is the whole point of Phase 3 for them.
func TestPersistentPreRunE_ScopeRemote_HTTPSProfile_Allowed(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles:\n  work:\n    url: https://work.example.com\n")
	writeRootTestTokenFile(t, "work", `{"device_id":"d","token":"tk_x","url":"https://work.example.com"}`)
	cmd := newProfileTestCmd(t, "work", map[string]string{scopeAnnotationKey: scopeRemote})

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("a scope=remote command against an https profile must not be rejected: %v", err)
	}
	c := client.FromContext(cmd.Context())
	if c == nil {
		t.Fatal("expected a client to have been injected")
	}
	if c.IsUnix() {
		t.Error("expected an https-scheme client to have been injected")
	}
}

// TestPersistentPreRunE_ScopeNeutral_HTTPSProfile_Allowed is a regression
// guard: scope=neutral's existing swallow behaviour (login/logout) must
// remain untouched by the new scope=local rejection branch — a successful
// resolution against an https profile is not an error at all, so it never
// reaches either the neutral-swallow or the local-rejection code paths, but
// this pins that adding the local check did not accidentally start
// rejecting neutral commands too.
func TestPersistentPreRunE_ScopeNeutral_HTTPSProfile_Allowed(t *testing.T) {
	writeRootTestConfigYAML(t, "profiles:\n  work:\n    url: https://work.example.com\n")
	writeRootTestTokenFile(t, "work", `{"device_id":"d","token":"tk_x","url":"https://work.example.com"}`)
	cmd := newProfileTestCmd(t, "work", map[string]string{scopeAnnotationKey: scopeNeutral})

	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("a scope=neutral command against an https profile must not be rejected: %v", err)
	}
	c := client.FromContext(cmd.Context())
	if c == nil {
		t.Fatal("expected a client to have been injected")
	}
	if c.IsUnix() {
		t.Error("expected an https-scheme client to have been injected")
	}
}

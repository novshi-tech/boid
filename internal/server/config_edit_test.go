package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/server"
)

// newConfigTestServer builds a *server.Server isolated under a fresh
// $XDG_CONFIG_HOME (server_test.go's own established isolation pattern),
// returning it alongside the config.yaml path buildRuntime resolved for it
// (config.DefaultPath() under the same isolated XDG_CONFIG_HOME) so tests
// can inspect the on-disk file ApplyConfigYAML writes.
func newConfigTestServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	configPath, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("config.DefaultPath: %v", err)
	}

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(t.TempDir(), "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })
	return srv, configPath
}

// TestConfigYAML_FreshInstall_IsEmpty pins ConfigYAML's sparse contract (see
// its own doc comment in config_edit.go): a fresh install with no
// config.yaml written yet returns an empty document, NOT a
// defaults-expanded one — the daemon still behaves per its built-in
// defaults at runtime (config.Load()/ValidateYAML always start from
// DefaultConfig()), only the on-disk/round-tripped representation is
// sparse.
func TestConfigYAML_FreshInstall_IsEmpty(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("ConfigYAML on a fresh install = %q, want empty", data)
	}
}

func TestConfigYAML_ReflectsWhatWasApplied(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.ApplyConfigYAML([]byte("sandbox:\n  backend: container\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if !strings.Contains(string(data), "backend: container") {
		t.Errorf("ConfigYAML output missing the applied sandbox.backend:\n%s", data)
	}
}

func TestApplyConfigYAML_PersistsToDisk(t *testing.T) {
	srv, configPath := newConfigTestServer(t)

	newDoc := []byte("sandbox:\n  allowed_domains:\n    - .example.com\n  backend: userns\n")
	if _, err := srv.ApplyConfigYAML(newDoc, "", true); err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}

	onDisk, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if string(onDisk) != string(newDoc) {
		t.Errorf("persisted config = %q, want %q", onDisk, newDoc)
	}

	// GET must reflect the just-applied state without a restart.
	got, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if !strings.Contains(string(got), ".example.com") {
		t.Errorf("ConfigYAML after apply missing the new domain:\n%s", got)
	}
}

func TestApplyConfigYAML_ValidationErrorLeavesStateUnchanged(t *testing.T) {
	srv, configPath := newConfigTestServer(t)

	before, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	if _, err := srv.ApplyConfigYAML([]byte("default_harness: claude-code\n"), "", true); err == nil {
		t.Fatal("expected validation error for unknown key default_harness")
	}

	after, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("live config changed despite a validation failure:\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("config.yaml should not have been written on validation failure (stat err=%v)", err)
	}
}

func TestApplyConfigYAML_AllowedDomains_HotReloadedNoWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("sandbox:\n  allowed_domains:\n    - .freee.co.jp\n    - .notion.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings for a dynamic-only change, got %v", result.Warnings)
	}

	// BLOCKER 2 (codex review round 1): the effective list is the built-in
	// floor UNION the just-applied user entries, not the user entries
	// alone — a hot-reload must never silently drop the built-in floor.
	got := srv.AllowedDomains()
	if !containsAll(got, ".freee.co.jp", ".notion.com") {
		t.Errorf("AllowedDomains() = %v, want it to contain the just-applied .freee.co.jp/.notion.com", got)
	}
	if !containsAll(got, config.DefaultAllowedDomains()...) {
		t.Errorf("AllowedDomains() = %v, want it to still contain every built-in floor domain", got)
	}
}

// TestApplyConfigYAML_AllowedDomains_RemovingUserEntryDropsIt pins BLOCKER
// 2's other half: removing a user-added domain from sandbox.allowed_domains
// must actually make it disappear from the effective list (not just from
// the sparse YAML) — the floor must be recomputed as
// config.DefaultAllowedDomains() ∪ (the new, narrower) user list, never as
// "whatever was effective before, plus/minus a diff".
func TestApplyConfigYAML_AllowedDomains_RemovingUserEntryDropsIt(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.ApplyConfigYAML([]byte("sandbox:\n  allowed_domains:\n    - .exfil.example\n    - .keep.example\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (add): %v", err)
	}
	if !containsAll(srv.AllowedDomains(), ".exfil.example") {
		t.Fatalf("precondition failed: .exfil.example not in AllowedDomains() after first apply")
	}

	if _, err := srv.ApplyConfigYAML([]byte("sandbox:\n  allowed_domains:\n    - .keep.example\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (remove): %v", err)
	}
	got := srv.AllowedDomains()
	for _, d := range got {
		if d == ".exfil.example" {
			t.Errorf("AllowedDomains() = %v, .exfil.example should have been removed by the second apply", got)
		}
	}
	if !containsAll(got, ".keep.example") {
		t.Errorf("AllowedDomains() = %v, .keep.example should still be present", got)
	}
	if !containsAll(got, config.DefaultAllowedDomains()...) {
		t.Errorf("AllowedDomains() = %v, built-in floor should still be present after removal", got)
	}
}

func containsAll(haystack []string, wants ...string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, w := range wants {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func TestApplyConfigYAML_NotifyAndPublicURL_HotReloadedNoWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("notify:\n  command: [\"/bin/true\"]\nweb:\n  public_url: https://boid.example.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings for notify.command/web.public_url (both dynamic), got %v", result.Warnings)
	}
}

func TestApplyConfigYAML_GatewayForges_RestartRequiredWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("gateway:\n  forges:\n    github:\n      secret_key: my-gh-pat\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly 1", result.Warnings)
	}
	// MINOR 2 (codex review round 1): the warning names the exact leaf
	// that changed (gateway.forges.github.secret_key), not just the forge
	// id (gateway.forges.github) — the plan doc's own example names the
	// leaf.
	want := "[warning] gateway.forges.github.secret_key requires daemon restart to take effect.\n" +
		"          Restart with: docker compose -f build/container/compose.yml restart daemon"
	if result.Warnings[0] != want {
		t.Errorf("warning text = %q, want %q", result.Warnings[0], want)
	}
}

// TestApplyConfigYAML_GatewayForges_MultipleLeafChanges pins MINOR 2: two
// leaves changing on the SAME forge id in one apply produce two separate,
// individually-named warnings.
func TestApplyConfigYAML_GatewayForges_MultipleLeafChanges(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.ApplyConfigYAML([]byte("gateway:\n  forges:\n    my-forge:\n      host: git.example.com\n      forge: github\n      secret_key: pat-1\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (create): %v", err)
	}

	result, err := srv.ApplyConfigYAML([]byte("gateway:\n  forges:\n    my-forge:\n      host: git2.example.com\n      forge: github\n      secret_key: pat-2\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML (change host+secret_key): %v", err)
	}
	var gotHost, gotSecret bool
	for _, w := range result.Warnings {
		if strings.Contains(w, "gateway.forges.my-forge.host requires") {
			gotHost = true
		}
		if strings.Contains(w, "gateway.forges.my-forge.secret_key requires") {
			gotSecret = true
		}
	}
	if !gotHost || !gotSecret {
		t.Errorf("Warnings = %v, want both a .host and a .secret_key leaf warning", result.Warnings)
	}
}

// TestApplyConfigYAML_RestartRequiredFields_AllWarn pins MAJOR 2 (codex
// review round 1): every schema leaf classified ReloadRestartRequired must
// produce a warning when changed, not just gateway.forges.* — the pre-fix
// applyDynamicConfigLocked hand-listed only two cases and silently ignored
// gc.*, web.http_addr, and task_ask.disconnect_grace.
func TestApplyConfigYAML_RestartRequiredFields_AllWarn(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"gc.enabled", "gc:\n  enabled: false\n", "gc.enabled"},
		{"gc.interval", "gc:\n  interval: 1h\n", "gc.interval"},
		{"gc.older_than", "gc:\n  older_than: 1h\n", "gc.older_than"},
		{"web.http_addr", "web:\n  http_addr: 127.0.0.1:9999\n", "web.http_addr"},
		{"task_ask.disconnect_grace", "task_ask:\n  disconnect_grace: 5m\n", "task_ask.disconnect_grace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newConfigTestServer(t)
			result, err := srv.ApplyConfigYAML([]byte(tc.doc), "", true)
			if err != nil {
				t.Fatalf("ApplyConfigYAML: %v", err)
			}
			found := false
			for _, w := range result.Warnings {
				if strings.Contains(w, tc.want+" requires daemon restart") {
					found = true
				}
			}
			if !found {
				t.Errorf("Warnings = %v, want a %q restart-required warning", result.Warnings, tc.want)
			}
		})
	}
}

// TestApplyConfigYAML_RestartRequiredFields_NoWarningWhenUnchanged pins the
// flip side: applying a document that doesn't actually change a
// restart-required field must not warn about it (only genuine changes do —
// same "warn on change, not on every apply" discipline the pre-fix
// sandbox.backend/gateway.forges handling already had).
func TestApplyConfigYAML_RestartRequiredFields_NoWarningWhenUnchanged(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.ApplyConfigYAML([]byte("gc:\n  enabled: false\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (first): %v", err)
	}
	// Second apply: gc.enabled unchanged (still false), only an unrelated
	// dynamic key changes.
	result, err := srv.ApplyConfigYAML([]byte("gc:\n  enabled: false\nweb:\n  public_url: https://x.example.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML (second): %v", err)
	}
	for _, w := range result.Warnings {
		if strings.Contains(w, "gc.enabled") {
			t.Errorf("unexpected gc.enabled warning when it did not change: %v", result.Warnings)
		}
	}
}

func TestApplyConfigYAML_SandboxBackend_RetirementWarningOnlyWhenChanged(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	// First apply: backend goes userns -> container. Warning fires.
	result, err := srv.ApplyConfigYAML([]byte("sandbox:\n  backend: container\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "retirement path") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected retirement warning on backend change, got %v", result.Warnings)
	}

	// Second apply: backend unchanged (still container), only an unrelated
	// key changes. No retirement warning should fire.
	result2, err := srv.ApplyConfigYAML([]byte("sandbox:\n  backend: container\n  allowed_domains:\n    - .example.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	for _, w := range result2.Warnings {
		if strings.Contains(w, "retirement path") {
			t.Errorf("unexpected retirement warning when sandbox.backend was unchanged: %v", result2.Warnings)
		}
	}
}

func TestApplyConfigYAML_ConcurrentApplies_NoTornFile(t *testing.T) {
	srv, configPath := newConfigTestServer(t)

	docs := [][]byte{
		[]byte("sandbox:\n  allowed_domains:\n    - .a.com\n"),
		[]byte("sandbox:\n  allowed_domains:\n    - .b.com\n"),
		[]byte("sandbox:\n  allowed_domains:\n    - .c.com\n"),
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(docs)*5)
	for i := 0; i < 5; i++ {
		for _, d := range docs {
			wg.Add(1)
			go func(doc []byte) {
				defer wg.Done()
				if _, err := srv.ApplyConfigYAML(doc, "", true); err != nil {
					errCh <- err
				}
			}(d)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("ApplyConfigYAML concurrent call failed: %v", err)
	}

	// The file on disk must be a single, fully-written, parseable
	// document — never a byte-interleaved mix of two concurrent writers.
	onDisk, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if _, err := config.ValidateYAML(onDisk); err != nil {
		t.Errorf("persisted config.yaml is not valid after concurrent applies: %v\ncontent:\n%s", err, onDisk)
	}
}

// TestMutateConfig_Set_ThenGet pins the server-side mutation endpoint
// (BLOCKER 1, codex review round 1): a single `op:"set"` call performs the
// whole read-modify-write under configMu, with no client-visible
// intermediate GET.
func TestMutateConfig_Set_ThenGet(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateSet, Key: "web.public_url", Value: []string{"https://boid.example.com"}})
	if err != nil {
		t.Fatalf("MutateConfig(set): %v", err)
	}
	if !strings.Contains(string(result.YAML), "https://boid.example.com") {
		t.Errorf("MutateConfig result YAML = %s, want it to contain the just-set value", result.YAML)
	}

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if !strings.Contains(string(data), "https://boid.example.com") {
		t.Errorf("ConfigYAML() = %s, want it to reflect the mutate", data)
	}
}

// TestMutateConfig_Unset pins the unset half of the mutation endpoint.
func TestMutateConfig_Unset(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateSet, Key: "web.public_url", Value: []string{"https://x.example.com"}}); err != nil {
		t.Fatalf("MutateConfig(set): %v", err)
	}
	if _, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateUnset, Key: "web.public_url"}); err != nil {
		t.Fatalf("MutateConfig(unset): %v", err)
	}

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if strings.Contains(string(data), "example.com") {
		t.Errorf("ConfigYAML() = %s, want web.public_url gone after unset", data)
	}
}

// TestMutateConfig_UnknownKey_Rejected pins the read-modify-write's
// validation: MutateConfig re-validates through the exact same
// config.Set/Unset dotted-path machinery `boid config set/unset` already
// used client-side — an unknown key is still rejected, not silently
// accepted into the document.
func TestMutateConfig_UnknownKey_Rejected(t *testing.T) {
	srv, _ := newConfigTestServer(t)
	if _, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateSet, Key: "sandbox.alowed_domains", Value: []string{"x"}}); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

// TestMutateConfig_ConcurrentSetsOfDifferentKeys_BothSucceed is the direct
// regression test for BLOCKER 1's failure scenario (codex review round 1):
// two concurrent `set`s of DIFFERENT keys, under the pre-fix
// GET→mutate→POST client flow, could interleave such that the second
// POST's stale snapshot silently reverted the first POST's already-applied
// change. Routing both through the server-side mutate endpoint (which holds
// configMu for its entire read-modify-write) must make both changes land,
// regardless of interleaving.
func TestMutateConfig_ConcurrentSetsOfDifferentKeys_BothSucceed(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateSet, Key: "notify.command", Value: []string{"/new-command"}}); err != nil {
			errCh <- err
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateSet, Key: "web.public_url", Value: []string{"https://new.example.com"}}); err != nil {
			errCh <- err
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent MutateConfig failed: %v", err)
	}

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "/new-command") {
		t.Errorf("final config missing notify.command change:\n%s", got)
	}
	if !strings.Contains(got, "https://new.example.com") {
		t.Errorf("final config missing web.public_url change (lost update — BLOCKER 1):\n%s", got)
	}
}

// TestApplyConfigYAML_IfMatch_MismatchRejected pins the ETag/If-Match
// concurrency guard `boid config edit`/`apply -f` (without --force) rely
// on: a stale revision is rejected, not silently overwritten.
func TestApplyConfigYAML_IfMatch_MismatchRejected(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	_, rev, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	// Someone else applies first, advancing the revision.
	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://other.example.com\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (other writer): %v", err)
	}

	// Our stale rev no longer matches.
	_, err = srv.ApplyConfigYAML([]byte("web:\n  public_url: https://mine.example.com\n"), rev, false)
	if err == nil {
		t.Fatal("expected a conflict error for a stale If-Match revision")
	}
	serr, ok := err.(*api.StatusError)
	if !ok {
		t.Fatalf("expected *api.StatusError, got %T: %v", err, err)
	}
	if serr.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want %d (Precondition Failed)", serr.Code, http.StatusPreconditionFailed)
	}

	// The conflicting apply must NOT have been persisted.
	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if strings.Contains(string(data), "mine.example.com") {
		t.Errorf("conflicting apply was persisted despite the If-Match mismatch:\n%s", data)
	}
}

// TestApplyConfigYAML_IfMatch_MissingRequiredUnlessForce pins the "428
// unless --force" half of the contract, mirroring
// (*ProjectAppService).UpdateWorkspace's existing If-Match convention
// (internal/api/project_service.go) exactly.
func TestApplyConfigYAML_IfMatch_MissingRequiredUnlessForce(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	_, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://x.example.com\n"), "", false)
	if err == nil {
		t.Fatal("expected an error when If-Match is missing and force is false")
	}
	serr, ok := err.(*api.StatusError)
	if !ok {
		t.Fatalf("expected *api.StatusError, got %T: %v", err, err)
	}
	if serr.Code != http.StatusPreconditionRequired {
		t.Errorf("status = %d, want %d (Precondition Required)", serr.Code, http.StatusPreconditionRequired)
	}

	// force=true bypasses the check entirely, even with no If-Match.
	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://x.example.com\n"), "", true); err != nil {
		t.Errorf("ApplyConfigYAML with force=true should succeed with no If-Match: %v", err)
	}
}

// TestApplyConfigYAML_IfMatch_MatchingRevisionSucceeds is the happy path:
// a fresh GET's revision round-tripped as If-Match applies cleanly.
func TestApplyConfigYAML_IfMatch_MatchingRevisionSucceeds(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	_, rev, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://x.example.com\n"), rev, false); err != nil {
		t.Errorf("ApplyConfigYAML with a fresh If-Match should succeed: %v", err)
	}
}

// TestConfigYAML_RevisionAdvancesOnEverySuccessfulApply pins the revision
// counter's own contract: every successful ApplyConfigYAML/MutateConfig
// bumps it, so a stale revision captured before a write is provably stale
// after.
func TestConfigYAML_RevisionAdvancesOnEverySuccessfulApply(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	_, rev1, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://x.example.com\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	_, rev2, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if rev1 == rev2 {
		t.Errorf("revision did not advance after a successful apply: rev1=%q rev2=%q", rev1, rev2)
	}
}

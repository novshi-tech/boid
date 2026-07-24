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

// TestApplyConfigYAML_AllowedDomains_RestartRequiredWarning pins the PR #830
// round-4 simplification (nose directive): sandbox.allowed_domains used to
// be hot-reloaded (ReloadDynamic) via a Server.AllowedDomains() method and a
// Runner.AllowedDomains func() []string getter — that machinery took 4
// codex review rounds to land safely and introduced a Server.Stop/dispatch
// deadlock along the way (round 4 blocker 2). It is ReloadRestartRequired
// now, same as everything else — see ReloadDynamic's own doc comment
// (internal/config/schema.go). Applying it just persists to config.yaml
// (asserted via ConfigYAML()) and warns; Server has no AllowedDomains()
// method left to observe a live value through at all.
func TestApplyConfigYAML_AllowedDomains_RestartRequiredWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("sandbox:\n  allowed_domains:\n    - .freee.co.jp\n    - .notion.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "sandbox.allowed_domains requires daemon restart") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want a sandbox.allowed_domains restart-required warning", result.Warnings)
	}

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if !strings.Contains(string(data), ".freee.co.jp") || !strings.Contains(string(data), ".notion.com") {
		t.Errorf("ConfigYAML() = %s, want it to contain the just-applied domains (persisted immediately even though not live yet)", data)
	}
}

// TestApplyConfigYAML_NotifyAndPublicURL_RestartRequiredWarning is
// notify.command/web.public_url's own half of the same PR #830 round-4
// simplification — both used to hot-apply via notify.Service.Update, now
// ReloadRestartRequired like everything else.
func TestApplyConfigYAML_NotifyAndPublicURL_RestartRequiredWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("notify:\n  command: [\"/bin/true\"]\nweb:\n  public_url: https://boid.example.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	var gotCommand, gotPublicURL bool
	for _, w := range result.Warnings {
		if strings.Contains(w, "notify.command requires daemon restart") {
			gotCommand = true
		}
		if strings.Contains(w, "web.public_url requires daemon restart") {
			gotPublicURL = true
		}
	}
	if !gotCommand || !gotPublicURL {
		t.Errorf("Warnings = %v, want both a notify.command and a web.public_url restart-required warning", result.Warnings)
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
		// PR #830 round-4 simplification (nose directive): these three were
		// ReloadDynamic before — see ReloadDynamic's own doc comment
		// (internal/config/schema.go).
		{"sandbox.allowed_domains", "sandbox:\n  allowed_domains:\n    - .example.com\n", "sandbox.allowed_domains"},
		{"notify.command", "notify:\n  command: [\"/bin/true\"]\n", "notify.command"},
		{"web.public_url", "web:\n  public_url: https://boid.example.com\n", "web.public_url"},
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
	// key changes.
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

// TestApplyConfigYAML_AllowedDomains_NoWarningWhenUnchanged is the
// array-valued counterpart of TestApplyConfigYAML_RestartRequiredFields_
// NoWarningWhenUnchanged: restartFieldExtractors renders
// sandbox.allowed_domains ([]string) by joining on "\x00" so it can be
// compared as a plain string like every scalar entry — this pins that two
// applies of the byte-identical slice are correctly seen as "unchanged"
// (not a false-positive warning on every apply, which a naive
// strings.Join(..., "") without a separator could produce for adjacent
// elements that happen to concatenate the same way).
func TestApplyConfigYAML_AllowedDomains_NoWarningWhenUnchanged(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	doc := []byte("sandbox:\n  allowed_domains:\n    - .a.example\n    - .b.example\n")
	if _, err := srv.ApplyConfigYAML(doc, "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (first): %v", err)
	}
	// Second apply: byte-identical sandbox.allowed_domains, only an
	// unrelated key changes.
	result, err := srv.ApplyConfigYAML([]byte(string(doc)+"web:\n  public_url: https://x.example.com\n"), "", true)
	if err != nil {
		t.Fatalf("ApplyConfigYAML (second): %v", err)
	}
	for _, w := range result.Warnings {
		if strings.Contains(w, "sandbox.allowed_domains") {
			t.Errorf("unexpected sandbox.allowed_domains warning when it did not change: %v", result.Warnings)
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

// TestMutateConfig_SequentialSingleOpsForNewForge_FailsOnFirstOp is the
// direct regression scenario for BLOCKER (codex review round 1, PR #831):
// three independent single-op MutateConfig calls that each fully set/unset
// ONE leaf of a brand-new forge id cannot ever create that forge, because
// each call validates the WHOLE document on its own — the very first call
// (setting only "corp.host") already leaves "corp.forge" empty, which fails
// validation before a second call ever runs. This pins the failure mode the
// batch path (below) exists to fix; it should keep failing even after the
// batch fix lands, since nothing about single-op semantics changed.
func TestMutateConfig_SequentialSingleOpsForNewForge_FailsOnFirstOp(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	_, err := srv.MutateConfig(api.ConfigMutateRequest{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.host", Value: []string{"git.corp.example"}})
	if err == nil {
		t.Fatal("expected the first single-op call to fail — a new forge with only \"host\" set is not yet a valid document")
	}
}

// TestMutateConfig_Batch_NewForgeAllFieldsAtOnce_Succeeds pins BLOCKER
// (codex review round 1, PR #831)'s fix: creating a brand-new custom forge
// (host + kind + secret_key) through the web settings form's three
// sequential leaf mutations failed validation on the very first call (see
// TestMutateConfig_SequentialSingleOpsForNewForge_FailsOnFirstOp). Batching
// all three ops into one MutateConfig call validates only once, after all
// three leaves are already in place, so it succeeds.
func TestMutateConfig_Batch_NewForgeAllFieldsAtOnce_Succeeds(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.MutateConfig(api.ConfigMutateRequest{Ops: []api.ConfigMutateRequest{
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.host", Value: []string{"git.corp.example"}},
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.forge", Value: []string{"github"}},
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.secret_key", Value: []string{"CORP_PAT"}},
	}})
	if err != nil {
		t.Fatalf("MutateConfig(batch, new forge): %v", err)
	}
	if !strings.Contains(string(result.YAML), "git.corp.example") || !strings.Contains(string(result.YAML), "CORP_PAT") {
		t.Errorf("MutateConfig result YAML = %s, want it to contain the new forge's host and secret_key", result.YAML)
	}

	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if !strings.Contains(string(data), "git.corp.example") {
		t.Errorf("ConfigYAML() = %s, want it to reflect the batched forge creation", data)
	}
}

// TestMutateConfig_Batch_ExistingLeafEditsSucceed pins the batch path's
// other everyday case (editing just one leaf of an ALREADY-valid, existing
// forge) — this already worked via a single op before the batch fix, and
// must keep working when routed through a one-element-equivalent batch too.
func TestMutateConfig_Batch_ExistingLeafEditsSucceed(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.MutateConfig(api.ConfigMutateRequest{Ops: []api.ConfigMutateRequest{
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.host", Value: []string{"git.corp.example"}},
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.forge", Value: []string{"github"}},
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.secret_key", Value: []string{"CORP_PAT_1"}},
	}}); err != nil {
		t.Fatalf("MutateConfig(batch, create): %v", err)
	}

	result, err := srv.MutateConfig(api.ConfigMutateRequest{Ops: []api.ConfigMutateRequest{
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.secret_key", Value: []string{"CORP_PAT_2"}},
	}})
	if err != nil {
		t.Fatalf("MutateConfig(batch, edit existing secret_key): %v", err)
	}
	if !strings.Contains(string(result.YAML), "CORP_PAT_2") {
		t.Errorf("MutateConfig result YAML = %s, want the updated secret_key", result.YAML)
	}
}

// TestMutateConfig_Batch_PartialFailureLeavesDocumentUnchanged pins the
// batch path's atomicity: when one op in the batch is invalid, NONE of the
// batch's earlier, individually-valid ops are persisted — the whole call
// fails together, and config.yaml on disk is untouched.
func TestMutateConfig_Batch_PartialFailureLeavesDocumentUnchanged(t *testing.T) {
	srv, configPath := newConfigTestServer(t)

	before, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read config before: %v", err)
	}

	_, err = srv.MutateConfig(api.ConfigMutateRequest{Ops: []api.ConfigMutateRequest{
		{Op: api.ConfigMutateSet, Key: "web.public_url", Value: []string{"https://should-not-land.example.com"}},
		{Op: api.ConfigMutateSet, Key: "sandbox.alowed_domains" /* typo */, Value: []string{"x"}},
	}})
	if err == nil {
		t.Fatal("expected the batch to fail because of the unknown key in its second op")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) && len(before) == 0 {
			return // still absent, matching the pre-call state — fine.
		}
		t.Fatalf("read config after: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("config.yaml changed despite the batch failing:\nbefore: %q\nafter:  %q", before, after)
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

// TestConfigYAML_RevisionIsContentDerived_SameContentSameRevision pins
// BLOCKER (codex review round 2)'s core property: the revision is a pure
// function of the document's bytes, so two independent reads of the SAME
// content — even across two calls that never share any in-memory state
// besides the file itself — produce the exact same revision. Two separate
// ApplyConfigYAML(..., force) calls that happen to write byte-identical
// content are used here as the "two independent reads" (rather than just
// calling ConfigYAML twice in a row, which would trivially pass even under
// the pre-fix counter design) to prove the value doesn't depend on how many
// writes happened before it, only on what's currently on disk.
func TestConfigYAML_RevisionIsContentDerived_SameContentSameRevision(t *testing.T) {
	srv, _ := newConfigTestServer(t)
	doc := []byte("web:\n  public_url: https://stable.example.com\n")

	if _, err := srv.ApplyConfigYAML(doc, "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (first): %v", err)
	}
	_, rev1, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	// Apply something else, then apply the SAME doc bytes again — the
	// revision must return to exactly rev1, not some new "third" value.
	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://other.example.com\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (other): %v", err)
	}
	if _, err := srv.ApplyConfigYAML(doc, "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (second, same bytes as first): %v", err)
	}
	_, rev2, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	if rev1 != rev2 {
		t.Errorf("revision for byte-identical content differs: rev1=%q rev2=%q (revision must be a pure function of content)", rev1, rev2)
	}
}

// TestConfigYAML_RevisionIsContentDerived_DifferentContentDifferentRevision
// is the flip side: two genuinely different documents must never produce
// the same revision.
func TestConfigYAML_RevisionIsContentDerived_DifferentContentDifferentRevision(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://a.example.com\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (a): %v", err)
	}
	_, revA, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	if _, err := srv.ApplyConfigYAML([]byte("web:\n  public_url: https://b.example.com\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML (b): %v", err)
	}
	_, revB, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	if revA == revB {
		t.Errorf("revision identical for different content: revA=%q revB=%q", revA, revB)
	}
}

// TestConfigYAML_RevisionStableAcrossSimulatedRestart is the direct
// regression test for BLOCKER (codex review round 2): a brand-new *Server
// process (simulating a daemon restart) reading the exact same on-disk
// config.yaml must report the exact same revision a pre-restart GET already
// reported — never re-baseline to some fresh "1", which is exactly what let
// editor A's stale If-Match alias writer B's already-applied change across
// a restart pre-fix (see computeRevision's own doc comment for the full
// failure scenario this closes).
func TestConfigYAML_RevisionStableAcrossSimulatedRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv1, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(t.TempDir(), "boid1.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("server.New (first process): %v", err)
	}
	t.Cleanup(func() { _ = srv1.DB().Close() })

	if _, err := srv1.ApplyConfigYAML([]byte("web:\n  public_url: https://restart-test.example.com\n"), "", true); err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	_, revBeforeRestart, err := srv1.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML (before restart): %v", err)
	}

	// Simulate a daemon restart: a brand-new *Server, same $XDG_CONFIG_HOME
	// (so config.DefaultPath() resolves to the exact same config.yaml
	// srv1 just wrote), no shared in-memory state with srv1 whatsoever.
	srv2, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(t.TempDir(), "boid2.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("server.New (second process, simulated restart): %v", err)
	}
	t.Cleanup(func() { _ = srv2.DB().Close() })

	_, revAfterRestart, err := srv2.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML (after restart): %v", err)
	}

	if revBeforeRestart != revAfterRestart {
		t.Errorf("revision changed across a simulated restart for identical content: before=%q after=%q — a stale If-Match captured before the restart would wrongly alias the post-restart document", revBeforeRestart, revAfterRestart)
	}
}

// TestApplyConfigYAML_ConcurrentApplies_SharedIfMatch_ExactlyOneSucceeds is
// the direct regression test for regression concern 1 (codex review round
// 2): the pre-existing TestApplyConfigYAML_ConcurrentApplies_NoTornFile only
// exercises force=true, so optimistic concurrency itself (the actual
// If-Match guard) was never tested under real concurrency, only
// sequentially (TestApplyConfigYAML_IfMatch_MismatchRejected). Two
// goroutines POST DIFFERENT content sharing the SAME starting If-Match,
// with force=false: exactly one must succeed (200), and the loser must be
// rejected with 412 Precondition Failed, never silently overwritten and
// never both silently applied.
func TestApplyConfigYAML_ConcurrentApplies_SharedIfMatch_ExactlyOneSucceeds(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	_, rev, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	docs := [][]byte{
		[]byte("web:\n  public_url: https://race-a.example.com\n"),
		[]byte("web:\n  public_url: https://race-b.example.com\n"),
	}

	var wg sync.WaitGroup
	errs := make([]error, len(docs))
	wg.Add(len(docs))
	for i, doc := range docs {
		i, doc := i, doc
		go func() {
			defer wg.Done()
			_, errs[i] = srv.ApplyConfigYAML(doc, rev, false)
		}()
	}
	wg.Wait()

	successes := 0
	var conflictErr error
	for _, err := range errs {
		if err == nil {
			successes++
		} else {
			conflictErr = err
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (a shared If-Match under concurrent applies must reject the loser, not apply both or neither)", successes)
	}
	if conflictErr == nil {
		t.Fatal("expected the losing apply to return a conflict error")
	}
	serr, ok := conflictErr.(*api.StatusError)
	if !ok {
		t.Fatalf("loser error type = %T, want *api.StatusError: %v", conflictErr, conflictErr)
	}
	if serr.Code != http.StatusPreconditionFailed {
		t.Errorf("loser status = %d, want %d (Precondition Failed)", serr.Code, http.StatusPreconditionFailed)
	}

	// Exactly one of the two documents must be persisted — never both,
	// never neither.
	data, _, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	got := string(data)
	hasA := strings.Contains(got, "race-a.example.com")
	hasB := strings.Contains(got, "race-b.example.com")
	if hasA == hasB {
		t.Errorf("final config = %q, want exactly one of race-a/race-b, got both=%v", got, hasA && hasB)
	}
}

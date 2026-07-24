package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

	data, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("ConfigYAML on a fresh install = %q, want empty", data)
	}
}

func TestConfigYAML_ReflectsWhatWasApplied(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	if _, err := srv.ApplyConfigYAML([]byte("sandbox:\n  backend: container\n")); err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}

	data, err := srv.ConfigYAML()
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
	if _, err := srv.ApplyConfigYAML(newDoc); err != nil {
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
	got, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}
	if !strings.Contains(string(got), ".example.com") {
		t.Errorf("ConfigYAML after apply missing the new domain:\n%s", got)
	}
}

func TestApplyConfigYAML_ValidationErrorLeavesStateUnchanged(t *testing.T) {
	srv, configPath := newConfigTestServer(t)

	before, err := srv.ConfigYAML()
	if err != nil {
		t.Fatalf("ConfigYAML: %v", err)
	}

	if _, err := srv.ApplyConfigYAML([]byte("default_harness: claude-code\n")); err == nil {
		t.Fatal("expected validation error for unknown key default_harness")
	}

	after, err := srv.ConfigYAML()
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

	result, err := srv.ApplyConfigYAML([]byte("sandbox:\n  allowed_domains:\n    - .freee.co.jp\n    - .notion.com\n"))
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings for a dynamic-only change, got %v", result.Warnings)
	}

	got := srv.AllowedDomains()
	want := []string{".freee.co.jp", ".notion.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("AllowedDomains() = %v, want %v (hot-reload did not take effect)", got, want)
	}
}

func TestApplyConfigYAML_NotifyAndPublicURL_HotReloadedNoWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("notify:\n  command: [\"/bin/true\"]\nweb:\n  public_url: https://boid.example.com\n"))
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings for notify.command/web.public_url (both dynamic), got %v", result.Warnings)
	}
}

func TestApplyConfigYAML_GatewayForges_RestartRequiredWarning(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	result, err := srv.ApplyConfigYAML([]byte("gateway:\n  forges:\n    github:\n      secret_key: my-gh-pat\n"))
	if err != nil {
		t.Fatalf("ApplyConfigYAML: %v", err)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly 1", result.Warnings)
	}
	want := "[warning] gateway.forges.github requires daemon restart to take effect.\n" +
		"          Restart with: docker compose -f build/container/compose.yml restart daemon"
	if result.Warnings[0] != want {
		t.Errorf("warning text = %q, want %q", result.Warnings[0], want)
	}
}

func TestApplyConfigYAML_SandboxBackend_RetirementWarningOnlyWhenChanged(t *testing.T) {
	srv, _ := newConfigTestServer(t)

	// First apply: backend goes userns -> container. Warning fires.
	result, err := srv.ApplyConfigYAML([]byte("sandbox:\n  backend: container\n"))
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
	result2, err := srv.ApplyConfigYAML([]byte("sandbox:\n  backend: container\n  allowed_domains:\n    - .example.com\n"))
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
				if _, err := srv.ApplyConfigYAML(doc); err != nil {
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

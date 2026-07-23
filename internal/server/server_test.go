package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/server"
)

func TestServer_StartAndStop(t *testing.T) {
	// Isolate from the real $XDG_CONFIG_HOME (see testutil.NewTestServer's
	// doc comment) — this test constructs *server.Server directly rather
	// than via testutil.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:         ":memory:",
		SocketPath:     sockPath,
		HTTPAddr:       "127.0.0.1:0",
		AllowedDomains: []string{"example.com"},
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Health check via UNIX socket
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := httpClient.Get("http://boid/api/health")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var health map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["status"] != "ok" {
		t.Errorf("health status = %q, want %q", health["status"], "ok")
	}
	if health["http_addr"] == "" {
		t.Errorf("health http_addr should be non-empty")
	}

	// Proxy API check
	proxyResp, err := httpClient.Get("http://boid/api/proxy")
	if err != nil {
		t.Fatalf("proxy check: %v", err)
	}
	defer proxyResp.Body.Close()

	var proxyInfo struct{ Port int }
	if err := json.NewDecoder(proxyResp.Body).Decode(&proxyInfo); err != nil {
		t.Fatalf("decode proxy: %v", err)
	}
	if proxyInfo.Port == 0 {
		t.Error("expected non-zero proxy port")
	}
	if srv.ProxyPort() != proxyInfo.Port {
		t.Errorf("ProxyPort() = %d, api returned %d", srv.ProxyPort(), proxyInfo.Port)
	}

	// Health check via TCP
	tcpAddr := srv.TCPAddr()
	if tcpAddr == "" {
		t.Fatal("expected non-empty TCP address")
	}

	tcpResp, err := http.Get("http://" + tcpAddr + "/api/health")
	if err != nil {
		t.Fatalf("tcp health check: %v", err)
	}
	defer tcpResp.Body.Close()

	if tcpResp.StatusCode != http.StatusOK {
		t.Errorf("tcp health status = %d, want %d", tcpResp.StatusCode, http.StatusOK)
	}

	// Broker socket check
	brokerSock := srv.BrokerSocket()
	if brokerSock == "" {
		t.Fatal("expected non-empty broker socket path")
	}

	// Stop
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify socket is cleaned up
	_, err = net.Dial("unix", sockPath)
	if err == nil {
		t.Error("expected error connecting to stopped server")
	}

	// Verify broker socket is cleaned up
	_, err = net.Dial("unix", brokerSock)
	if err == nil {
		t.Error("expected error connecting to stopped broker")
	}
}

// TestServer_KitsDir_ReturnsAbsolutePath pins codex PR7 review round 3's
// MAJOR: --kits-dir accepts a relative path and stores it verbatim in
// cfg.KitsDir. Before this fix, the endpoint returned that raw value, so a
// CLI running in a different cwd from the daemon would resolve the
// relative path against ITS OWN cwd and pull kits from an entirely
// different directory (possibly nonexistent), silently materializing the
// wrong runtime into the workspace. Server.KitsDir() must normalize to an
// absolute path so cwd differences cannot skew resolution.
func TestServer_KitsDir_ReturnsAbsolutePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "boid.sock")

	// Relative path — exactly what "boid start --kits-dir some/dir" would
	// store verbatim in cfg.KitsDir.
	relKits := "some/relative/kits"

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    relKits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	got := srv.KitsDir()
	if !filepath.IsAbs(got) {
		t.Errorf("KitsDir() = %q, want an absolute path (relative --kits-dir must be normalized)", got)
	}
}

// TestServer_KitsDir_PreservesAbsolutePath is the regression counterpart:
// an already-absolute --kits-dir must be returned unchanged (not
// double-joined onto another cwd).
func TestServer_KitsDir_PreservesAbsolutePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "boid.sock")
	absKits := filepath.Join(tmp, "absolute", "kits")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    absKits,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if got := srv.KitsDir(); got != absKits {
		t.Errorf("KitsDir() = %q, want %q (absolute path must not be re-joined)", got, absKits)
	}
}

// TestServer_KitsDir_EmptyStaysEmpty pins that empty (unconfigured) KitsDir
// renders as empty, not filepath.Abs("") which would resolve to the daemon
// cwd — the empty state must stay observably empty for the CLI's fallback
// / hard-error branching to still work.
func TestServer_KitsDir_EmptyStaysEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if got := srv.KitsDir(); got != "" {
		t.Errorf("KitsDir() = %q, want empty (empty must not be turned into daemon cwd)", got)
	}
}

// TestServer_New_InstallIDLoadFailure_Advisory pins Major 5 (PR6 codex
// review): install_id is a container-backend concept (§決定6's
// boid.install_id docker resource label / `boid reap`'s label filter) that
// the userns backend never touches, so a failure loading/creating it must
// not block New() at all — a userns daemon whose InstallIDDir happens to
// be unwritable/unreadable (e.g. left root-owned by a prior run under a
// different uid) must still be able to start. New() logs a warning and
// continues with an empty InstallID() instead of the pre-fix behavior
// (close the DB and return the error, refusing to start).
func TestServer_New_InstallIDLoadFailure_Advisory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A regular FILE (not a directory) at the InstallIDDir path makes
	// install.LoadOrCreate's internal os.ReadFile(dir/install_id) fail
	// with ENOTDIR — a portable, deterministic way to force the load
	// error without relying on root/permission tricks that would not work
	// the same way in every CI environment.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	srv, err := server.New(server.Config{
		DBPath:       ":memory:",
		SocketPath:   filepath.Join(t.TempDir(), "boid.sock"),
		HTTPAddr:     "127.0.0.1:0",
		InstallIDDir: notADir,
	})
	if err != nil {
		t.Fatalf("New() should not fail on an install_id load error, got: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if id := srv.InstallID(); id != "" {
		t.Errorf("InstallID() = %q, want empty (advisory failure must not fabricate an id)", id)
	}
}

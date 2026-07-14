package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/gitgateway"
)

// TestGitGateway_NoSecretStoreRejectsValidTokenWithout503 is the
// internal-package (reaches srv.gatewayRegistry directly) end-to-end guard
// for docs/plans/git-gateway-cutover.md PR5's "KeyFilePath 未設定時の
// CredentialError 抑制" fix: a daemon started without config.KeyFilePath
// (so Server never builds a *dispatcher.SecretStore — see server.go's
// `if cfg.KeyFilePath != ""` guard) must reject an otherwise valid,
// authorized gateway request with 503 rather than silently forwarding it
// unauthenticated. internal/gitgateway's own TestServeHTTP_NoResolverConfiguredRejectsWithoutNotifying
// covers the notifier-suppression half of this in isolation; this test
// proves the real internal/server/wire.go wiring (gwResolver == nil when
// secretStore == nil) actually reaches that code path end to end.
func TestGitGateway_NoSecretStoreRejectsValidTokenWithout503(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		// KeyFilePath deliberately left unset: no secret store.
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if srv.gatewayRegistry == nil {
		t.Fatal("srv.gatewayRegistry is nil after Start")
	}
	repoKey := gitgateway.NewRepoKey("github.com", "owner", "repo")
	token := srv.gatewayRegistry.Register(map[gitgateway.RepoKey]gitgateway.Permission{
		repoKey: gitgateway.PermFetch,
	}, "default")

	gwURL := srv.GatewayURL()
	const wantPrefix = "http://10.0.2.2:"
	if len(gwURL) <= len(wantPrefix) {
		t.Fatalf("GatewayURL = %q, want non-empty http://10.0.2.2:<port>", gwURL)
	}
	port := gwURL[len(wantPrefix):]

	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://127.0.0.1:" + port + "/j/" + token + "/github.com/owner/repo.git/info/refs?service=git-upload-pack"
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET against bound gateway listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (valid token/repo, but no secret store configured)", resp.StatusCode)
	}
}

package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/server"
)

func newTestClient(sockPath string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
}

func TestWebUI_EnabledByDefault(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	// Web UI が有効なので TCP listener が開かれること
	if addr := srv.TCPAddr(); addr == "" {
		t.Error("TCP listener should be open when Web UI is enabled by default")
	}

	client := newTestClient(sockPath)

	// Web UI が有効なら / は HTML を返すこと (404 でないこと)
	resp, err := client.Get("http://boid/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("GET /: got 404, expected Web UI to be enabled; body: %q", string(body))
	}

	// JSON API も引き続き動作すること
	resp, err = client.Get("http://boid/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/health: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

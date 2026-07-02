package testutil

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/server"
)

// TestServer wraps a running server for testing.
type TestServer struct {
	Server *server.Server
	Client *client.Client
}

// NewTestServer starts a server with a temp UNIX socket and in-memory DB.
func NewTestServer(t *testing.T) *TestServer {
	t.Helper()

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() { _ = srv.Stop() })

	return &TestServer{
		Server: srv,
		Client: client.NewUnixClient(sockPath),
	}
}

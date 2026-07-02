package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestStopDoesNotRemoveForeignSocket pins the fix for the daemon-restart-resume
// flake: Server.Stop() must never remove a socket file at cfg.SocketPath that it
// does not own.
//
// The failure mode: on a fast restart, httpServer.Close() unlinks our own socket
// early (net.UnixListener.Close unlinks via the fd it owns), so a successor
// daemon can create a brand-new socket at the same path (tmpfs even reuses the
// inode) while this Stop() is still draining. A trailing, blind
// os.Remove(cfg.SocketPath) would then delete the successor's *live* socket,
// leaving clients with ENOENT.
//
// This test models the tail of Stop() — our own listener already gone
// (httpServer nil) and only cleanup remaining — with a successor socket sitting
// at the path. Stop() must leave it intact.
func TestStopDoesNotRemoveForeignSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "boid.sock")

	// A successor daemon's live socket already listening at the shared path.
	successor, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen successor: %v", err)
	}
	defer successor.Close()

	// A server with no listener of its own (all lifecycle fields nil). Every
	// branch in Stop() is nil-guarded, so this exercises the socket-cleanup tail
	// in isolation.
	srv := &Server{cfg: Config{SocketPath: sockPath}}
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("Stop removed a socket it does not own (daemon-restart-resume flake): %v", err)
	}
	// The successor's socket must still be connectable.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("successor socket no longer connectable after Stop: %v", err)
	}
	conn.Close()
}

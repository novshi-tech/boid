package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// NoAutostartEnv disables automatic server startup when set to "1".
	NoAutostartEnv = "BOID_NO_AUTOSTART"

	socketProbeTimeout = 200 * time.Millisecond
)

// EnsureRunning ensures the boid server is running, starting it automatically
// if it is not. It uses a file lock to prevent concurrent start races.
//
// If BOID_NO_AUTOSTART=1, auto-start is skipped and an error is returned when
// the server is not reachable.
func EnsureRunning(ctx context.Context) error {
	return ensureRunning(ctx, DefaultSocketPath(), autostartLockPath(), spawnServer)
}

// ensureRunning is the testable implementation with injectable dependencies.
func ensureRunning(ctx context.Context, socketPath, lockPath string, spawner func(context.Context, string) error) error {
	// Fast path: server is already up.
	if isSocketReady(socketPath) {
		return nil
	}

	if os.Getenv(NoAutostartEnv) == "1" {
		return fmt.Errorf("boid server is not running; start it with 'boid start' or check logs at %s", autostartLogPath())
	}

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create autostart lock dir: %w", err)
	}

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open autostart lock: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire autostart lock: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	// Re-check after acquiring lock: another process may have started the server.
	if isSocketReady(socketPath) {
		return nil
	}

	if err := spawner(ctx, socketPath); err != nil {
		return err
	}

	return nil
}

// spawnServer starts the boid server by re-executing the current binary with
// the "start" subcommand, and waits for the parent phase to finish.
// boid start's parent phase blocks until the socket is ready, so when Wait
// returns with nil the socket is guaranteed to be available.
func spawnServer(ctx context.Context, socketPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve boid executable: %w", err)
	}

	cmd := exec.CommandContext(ctx, exe, "start")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start boid server: %w", err)
	}

	// boid start's parent phase waits for the UNIX socket to become ready
	// before exiting. Waiting here guarantees the socket is up on return.
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("boid server failed to start: %w; run 'boid start' manually or check logs at %s", err, autostartLogPath())
	}

	return nil
}

func isSocketReady(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, socketProbeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func autostartLockPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "boid", "autostart.lock")
	}
	return fmt.Sprintf("/tmp/boid-%d-autostart.lock", os.Getuid())
}

func autostartLogPath() string {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateDir, "boid", "boid.log")
}

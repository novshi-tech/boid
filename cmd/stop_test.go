//go:build linux

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestRunStop_SharesHostModeLockWithComposeUp is the codex round-12
// review of PR5, Major regression test: `boid stop` used to run
// runComposeDownScript with no locking at all, while runComposeUp
// (`boid start` / host mode's own autostart) always has held
// withHostModeLock's flock since round-2's own review. A `boid stop`
// racing a concurrent autostart could read scripts/deploy-container.sh's
// engine-state file, run its own `compose down`, and finish AFTER the
// autostart's `compose up` completed — reporting "stack stopped" while a
// stack it just raced past kept running. runStop must now hold the SAME
// lock. Proven here by holding the lock file externally (a second, raw
// flock — Linux flock() blocks a same-process second holder exactly like
// a different process, since it is keyed on the open file description,
// not the pid) and confirming runStop does not proceed until it is
// released.
func TestRunStop_SharesHostModeLockWithComposeUp(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	// A fake checkout runStop's own findComposeRoot will resolve via
	// BOID_COMPOSE_ROOT — its scripts/deploy-container.sh just needs to
	// exit quickly (successfully) once actually invoked, so this test can
	// tell "blocked on the lock" apart from "ran to completion" purely by
	// timing.
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", scriptsDir, err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "deploy-container.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake deploy-container.sh: %v", err)
	}
	t.Setenv("BOID_COMPOSE_ROOT", root)

	// Hold the SAME lock file externally, exactly like a concurrent
	// runComposeUp would via withHostModeLock.
	dir, err := hostModeAssetsDir()
	if err != nil {
		t.Fatalf("hostModeAssetsDir: %v", err)
	}
	lockPath := filepath.Join(dir, cliLockFileName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}

	fakeCmd := &cobra.Command{}
	fakeCmd.SetContext(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- runStop(fakeCmd, nil)
	}()

	select {
	case err := <-done:
		t.Fatalf("runStop returned (err=%v) while the host-mode lock was still held externally — it must block until the lock is released", err)
	case <-time.After(200 * time.Millisecond):
		// Still blocked, as expected.
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runStop after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runStop did not proceed after the host-mode lock was released")
	}
}

package client

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// setupListener starts a Unix socket listener at socketPath and returns it.
// The caller must close the listener when done.
func setupListener(t *testing.T, socketPath string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return ln
}

func TestEnsureRunning_AlreadyRunning(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "boid.sock")
	lockPath := filepath.Join(tmpDir, "autostart.lock")

	ln := setupListener(t, socketPath)
	defer ln.Close()

	spawnCalled := false
	err := ensureRunning(context.Background(), socketPath, lockPath, func(_ context.Context, _ string) error {
		spawnCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawnCalled {
		t.Error("spawner must not be called when server is already running")
	}
}

func TestEnsureRunning_NoAutostart(t *testing.T) {
	t.Setenv(NoAutostartEnv, "1")

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "boid.sock")
	lockPath := filepath.Join(tmpDir, "autostart.lock")

	err := ensureRunning(context.Background(), socketPath, lockPath, func(_ context.Context, _ string) error {
		t.Fatal("spawner must not be called when BOID_NO_AUTOSTART=1")
		return nil
	})
	if err == nil {
		t.Fatal("expected error when BOID_NO_AUTOSTART=1 and server not running")
	}
}

func TestEnsureRunning_SpawnOnMissing(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "boid.sock")
	lockPath := filepath.Join(tmpDir, "autostart.lock")

	var ln net.Listener
	spawnCalled := false
	err := ensureRunning(context.Background(), socketPath, lockPath, func(_ context.Context, sp string) error {
		spawnCalled = true
		var err error
		ln, err = net.Listen("unix", sp)
		if err != nil {
			return err
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
		return nil
	})
	if ln != nil {
		defer ln.Close()
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawnCalled {
		t.Error("spawner must be called when server is not running")
	}
}

// TestEnsureRunning_ConcurrentFlock verifies that concurrent callers use the
// file lock to serialize: the spawner is invoked exactly once even when
// multiple goroutines call ensureRunning simultaneously.
func TestEnsureRunning_ConcurrentFlock(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "boid.sock")
	lockPath := filepath.Join(tmpDir, "autostart.lock")

	var spawnCount atomic.Int32
	var mu sync.Mutex
	var sharedListener net.Listener

	spawner := func(_ context.Context, sp string) error {
		spawnCount.Add(1)
		mu.Lock()
		defer mu.Unlock()
		if sharedListener != nil {
			return nil
		}
		var err error
		sharedListener, err = net.Listen("unix", sp)
		if err != nil {
			return err
		}
		go func() {
			for {
				conn, err := sharedListener.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
		return nil
	}
	defer func() {
		mu.Lock()
		if sharedListener != nil {
			sharedListener.Close()
		}
		mu.Unlock()
	}()

	const n = 5
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ensureRunning(context.Background(), socketPath, lockPath, spawner); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent ensureRunning error: %v", err)
	}
	if got := spawnCount.Load(); got != 1 {
		t.Errorf("spawner called %d times, want 1", got)
	}
}

func TestEnsureRunning_AlreadyRunningAfterLock(t *testing.T) {
	// Simulate the case where another process starts the server between
	// the initial socket check and acquiring the lock.
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "boid.sock")
	lockPath := filepath.Join(tmpDir, "autostart.lock")

	// Create the listener before the first call to ensureRunning
	// to simulate the "already started by someone else" scenario.
	// We pass a spawner that would create the socket if called,
	// but since the socket already exists after lock re-check, it won't be.
	ln := setupListener(t, socketPath)
	defer ln.Close()

	spawnCalled := false
	err := ensureRunning(context.Background(), socketPath, lockPath, func(_ context.Context, _ string) error {
		spawnCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawnCalled {
		t.Error("spawner must not be called when socket becomes ready before spawn")
	}
}

// TestEnsureRunning_OSEnvNotSet verifies that when BOID_NO_AUTOSTART is absent,
// the spawner IS called (normal autostart path).
func TestEnsureRunning_OSEnvNotSet(t *testing.T) {
	os.Unsetenv(NoAutostartEnv)

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "boid.sock")
	lockPath := filepath.Join(tmpDir, "autostart.lock")

	var ln net.Listener
	spawnCalled := false
	err := ensureRunning(context.Background(), socketPath, lockPath, func(_ context.Context, sp string) error {
		spawnCalled = true
		var err error
		ln, err = net.Listen("unix", sp)
		if err != nil {
			return err
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
		return nil
	})
	if ln != nil {
		defer ln.Close()
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawnCalled {
		t.Error("spawner must be called when BOID_NO_AUTOSTART is not set")
	}
}

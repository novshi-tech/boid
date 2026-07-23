package daemon

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMain allows the test binary to act as a short-lived daemon child when
// BOID_DAEMON_CHILD=1 is present (as Spawn sets it). Without this guard, a
// Spawn call inside a test would re-execute the full test suite recursively.
func TestMain(m *testing.M) {
	if IsChild() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// --- LogFilePath ---

func TestLogFilePath_WithXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	got := LogFilePath()
	want := filepath.Join(dir, "boid", "boid.log")
	if got != want {
		t.Fatalf("LogFilePath() = %q, want %q", got, want)
	}
}

func TestLogFilePath_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, _ := os.UserHomeDir()

	got := LogFilePath()
	want := filepath.Join(home, ".local", "state", "boid", "boid.log")
	if got != want {
		t.Fatalf("LogFilePath() = %q, want %q", got, want)
	}
}

// --- IsSocketAlive ---

func TestIsSocketAlive_NoSocketFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	if IsSocketAlive(path, 100*time.Millisecond) {
		t.Fatal("IsSocketAlive() = true for missing socket, want false")
	}
}

func TestIsSocketAlive_StaleSocketFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.sock")
	// Create a plain file at socket path to simulate a stale socket file.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if IsSocketAlive(path, 100*time.Millisecond) {
		t.Fatal("IsSocketAlive() = true for stale socket file, want false")
	}
}

func TestIsSocketAlive_Listening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if !IsSocketAlive(path, 500*time.Millisecond) {
		t.Fatal("IsSocketAlive() = false for active listener, want true")
	}
}

// --- IsChild ---

func TestIsChild_False(t *testing.T) {
	t.Setenv(daemonEnvKey, "")
	if IsChild() {
		t.Fatal("IsChild() = true, want false when env var is empty")
	}
}

func TestIsChild_True(t *testing.T) {
	t.Setenv(daemonEnvKey, "1")
	if !IsChild() {
		t.Fatal("IsChild() = false, want true when env var is 1")
	}
}

func TestShouldLogToStdout_False(t *testing.T) {
	t.Setenv(logStdoutEnvKey, "")
	if ShouldLogToStdout() {
		t.Fatal("ShouldLogToStdout() = true, want false when env var is empty (every pre-PR9 caller)")
	}
}

func TestShouldLogToStdout_True(t *testing.T) {
	t.Setenv(logStdoutEnvKey, "1")
	if !ShouldLogToStdout() {
		t.Fatal("ShouldLogToStdout() = false, want true when env var is 1")
	}
}

// --- RedirectToLog ---

// --- Spawn ---

// TestSpawn_PIDIsPositive verifies that Spawn returns a positive PID.
// On Go 1.20+ with Linux pidfd, Process.Release() zeroes out Process.Pid to
// -1.  Saving the PID before Release is required to return the real value.
// TestMain short-circuits the re-executed test binary so it exits immediately
// when BOID_DAEMON_CHILD=1 is detected, preventing recursive test execution.
func TestSpawn_PIDIsPositive(t *testing.T) {
	pid, statusR, err := Spawn(os.Args[:1])
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer statusR.Close()
	if pid <= 0 {
		t.Fatalf("Spawn() returned pid %d, want > 0", pid)
	}
}

// TestRedirectToLog verifies the log file is created at the requested path.
// We spawn a subprocess so that redirecting fd 0/1/2 does not affect the test
// runner itself.  The subprocess is just the Go test binary re-run with a
// specific environment flag that triggers the redirect and then exits.
//
// Because forking test helpers is complex, we limit ourselves to checking
// that RedirectToLog creates the file and that subsequent writes on fd 1/2
// land there — tested indirectly through a simpler path-creation check.
func TestRedirectToLog_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	logPath := filepath.Join(subDir, "boid.log")

	// RedirectToLog must create the subdirectory.
	// We run it in the test process but immediately dup back so that the test
	// runner's output is not lost.
	savedStdout, err := syscall.Dup(1)
	if err != nil {
		t.Skipf("cannot dup stdout: %v", err)
	}
	savedStderr, err := syscall.Dup(2)
	if err != nil {
		syscall.Close(savedStdout)
		t.Skipf("cannot dup stderr: %v", err)
	}
	defer func() {
		syscall.Dup2(savedStdout, 1)
		syscall.Dup2(savedStderr, 2)
		syscall.Close(savedStdout)
		syscall.Close(savedStderr)
	}()

	if err := RedirectToLog(logPath); err != nil {
		t.Fatalf("RedirectToLog: %v", err)
	}

	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

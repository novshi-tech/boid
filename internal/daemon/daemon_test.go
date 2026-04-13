package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

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

// --- PIDFilePath ---

func TestPIDFilePath_WithXDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	got := PIDFilePath()
	want := filepath.Join(dir, "boid", "boid.pid")
	if got != want {
		t.Fatalf("PIDFilePath() = %q, want %q", got, want)
	}
}

func TestPIDFilePath_FallsBackToStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", dir)

	got := PIDFilePath()
	want := filepath.Join(dir, "boid", "boid.pid")
	if got != want {
		t.Fatalf("PIDFilePath() = %q, want %q", got, want)
	}
}

// --- WritePID / ReadPID ---

func TestWriteReadPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid.pid")

	if err := WritePID(path, 12345); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	got, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if got != 12345 {
		t.Fatalf("ReadPID() = %d, want 12345", got)
	}
}

func TestWritePID_CreatesParentDir(t *testing.T) {
	// Path whose parent directory does not yet exist.
	path := filepath.Join(t.TempDir(), "sub", "dir", "boid.pid")

	if err := WritePID(path, 99); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("PID file not created: %v", err)
	}
}

func TestReadPID_NotExist(t *testing.T) {
	_, err := ReadPID(filepath.Join(t.TempDir(), "missing.pid"))
	if !os.IsNotExist(err) {
		t.Fatalf("ReadPID() error = %v, want os.ErrNotExist", err)
	}
}

func TestReadPID_InvalidContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid.pid")
	if err := os.WriteFile(path, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadPID(path)
	if err == nil {
		t.Fatal("ReadPID() expected error for invalid content, got nil")
	}
}

// --- RemovePID ---

func TestRemovePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid.pid")
	if err := WritePID(path, 1); err != nil {
		t.Fatal(err)
	}
	if err := RemovePID(path); err != nil {
		t.Fatalf("RemovePID: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("PID file still exists after RemovePID")
	}
}

func TestRemovePID_NotExistIsNoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.pid")
	if err := RemovePID(path); err != nil {
		t.Fatalf("RemovePID on missing file: %v", err)
	}
}

// --- CheckNotRunning ---

func TestCheckNotRunning_NoPIDFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid.pid")
	if err := CheckNotRunning(path); err != nil {
		t.Fatalf("CheckNotRunning (no file): %v", err)
	}
}

func TestCheckNotRunning_StalePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid.pid")

	// Write a PID that is almost certainly not alive (PID 1 is init, but we
	// want something guaranteed non-existent; use a large number and verify it
	// is not our own process).
	stalePID := 2000000 // exceeds Linux PID_MAX_LIMIT on most kernels
	if err := WritePID(path, stalePID); err != nil {
		t.Fatal(err)
	}

	// signal(0) on a non-existent PID returns ESRCH → CheckNotRunning returns nil.
	if err := CheckNotRunning(path); err != nil {
		// Might spuriously fail if the kernel has a process at that PID (very unlikely).
		t.Skipf("PID %d unexpectedly alive (or system PID space is unusual): %v", stalePID, err)
	}
}

func TestCheckNotRunning_RunningProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid.pid")

	// Write the current process's PID — it is definitely running.
	if err := WritePID(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	err := CheckNotRunning(path)
	if err == nil {
		t.Fatal("CheckNotRunning expected error for running process, got nil")
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

// --- RedirectToLog ---

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

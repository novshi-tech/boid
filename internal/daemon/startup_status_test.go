package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fd3Guard installs f at the exact descriptor number the status-pipe helpers
// hard-code (statusPipeFD), so a test can observe whether those helpers close
// it. It returns a restore func that puts the previous fd 3 back.
func fd3Guard(t *testing.T, f *os.File) func() {
	t.Helper()
	saved, dupErr := syscall.Dup(statusPipeFD)
	hadFD3 := dupErr == nil
	if err := syscall.Dup3(int(f.Fd()), statusPipeFD, 0); err != nil {
		t.Fatalf("dup3 onto fd %d: %v", statusPipeFD, err)
	}
	return func() {
		if hadFD3 {
			_ = syscall.Dup3(saved, statusPipeFD, 0)
			_ = syscall.Close(saved)
			return
		}
		_ = syscall.Close(statusPipeFD)
	}
}

// fd3Open reports whether statusPipeFD is still a valid descriptor.
func fd3Open() bool {
	var st syscall.Stat_t
	return syscall.Fstat(statusPipeFD, &st) == nil
}

// TestCloseStartupFD3_WithoutSpawnEnv_LeavesFD3Open pins the fix for the
// 2026-07-31 container-daemon incident: when the process was NOT launched by
// Spawn (build/container/compose.yml runs `boid start` directly with
// BOID_DAEMON_CHILD=1, so nothing is wired onto fd 3), whatever happens to
// occupy fd 3 is unrelated to the status pipe. In that incident SQLite took
// fd 3 for boid.db inside server.New, and this helper closed it the moment
// startup succeeded — every later query failed with SQLITE_IOERR_FSTAT /
// SQLITE_CORRUPT against a descriptor that was already EBADF.
func TestCloseStartupFD3_WithoutSpawnEnv_LeavesFD3Open(t *testing.T) {
	t.Setenv(statusPipeEnvKey, "")
	os.Unsetenv(statusPipeEnvKey)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	restore := fd3Guard(t, w)
	defer restore()

	CloseStartupFD3()

	if !fd3Open() {
		t.Fatal("CloseStartupFD3 closed fd 3 although this process was not launched by Spawn; an unrelated file (e.g. the SQLite DB) landing on fd 3 would be destroyed")
	}
}

// TestCloseStartupFD3_WithSpawnEnv_ClosesFD3 keeps the original contract
// intact for the Spawn path: the parent's read-end must observe EOF, which
// only happens if the child really closes its write-end.
func TestCloseStartupFD3_WithSpawnEnv_ClosesFD3(t *testing.T) {
	t.Setenv(statusPipeEnvKey, "1")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	restore := fd3Guard(t, w)
	defer restore()

	CloseStartupFD3()

	if fd3Open() {
		t.Fatal("CloseStartupFD3 left fd 3 open under the Spawn env; the parent would block instead of seeing EOF")
	}
}

// TestWriteStartupStatusOnFD3_WithoutSpawnEnv_LeavesFD3Open covers the
// failure-path twin of the above: the error reporter must not write into —
// or close — a descriptor that is not the status pipe.
func TestWriteStartupStatusOnFD3_WithoutSpawnEnv_LeavesFD3Open(t *testing.T) {
	t.Setenv(statusPipeEnvKey, "")
	os.Unsetenv(statusPipeEnvKey)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	restore := fd3Guard(t, w)
	defer restore()

	WriteStartupStatusOnFD3(errors.New("boom"))

	if !fd3Open() {
		t.Fatal("WriteStartupStatusOnFD3 closed fd 3 although this process was not launched by Spawn")
	}
}

// TestSpawnEnv_MarksStatusPipe asserts Spawn is the one place that declares
// fd 3 to be the status pipe — the env var the helpers above gate on.
func TestSpawnEnv_MarksStatusPipe(t *testing.T) {
	got := spawnEnv([]string{"PATH=/usr/bin"})
	if !slices.Contains(got, daemonEnvKey+"=1") {
		t.Fatalf("spawnEnv missing %s=1: %v", daemonEnvKey, got)
	}
	if !slices.Contains(got, statusPipeEnvKey+"=1") {
		t.Fatalf("spawnEnv missing %s=1: %v", statusPipeEnvKey, got)
	}
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Fatalf("spawnEnv dropped the base environment: %v", got)
	}
}

// TestBuildStartupStatus_Migration verifies that a
// *orchestrator.ProjectMigrationError (even wrapped multiple times) is
// recovered and serialised into Kind=migration with every issue preserved
// in order.
func TestBuildStartupStatus_Migration(t *testing.T) {
	mig := &orchestrator.ProjectMigrationError{
		Projects: []orchestrator.ProjectMigrationIssue{
			{ProjectID: "id1", Dir: "/a", Messages: []string{"m1"}},
			{ProjectID: "id2", Dir: "/b", Messages: []string{"m2a", "m2b"}},
		},
	}
	wrapped := fmt.Errorf("create server: %w", mig)

	got := buildStartupStatus(wrapped)
	if got.Kind != StartupKindMigration {
		t.Fatalf("Kind = %q, want %q", got.Kind, StartupKindMigration)
	}
	if len(got.Projects) != 2 {
		t.Fatalf("Projects len = %d, want 2", len(got.Projects))
	}
	if got.Projects[0].ID != "id1" || got.Projects[0].Dir != "/a" {
		t.Fatalf("project[0] = %+v", got.Projects[0])
	}
	if got.Projects[1].Messages[1] != "m2b" {
		t.Fatalf("project[1].Messages[1] = %q, want %q", got.Projects[1].Messages[1], "m2b")
	}
}

// TestBuildStartupStatus_Other captures the fallback path for non-migration
// startup errors. The message must echo err.Error() verbatim so the parent
// can pass it through unchanged.
func TestBuildStartupStatus_Other(t *testing.T) {
	plain := errors.New("disk full")
	got := buildStartupStatus(plain)
	if got.Kind != StartupKindOther {
		t.Fatalf("Kind = %q, want %q", got.Kind, StartupKindOther)
	}
	if got.Message != "disk full" {
		t.Fatalf("Message = %q, want %q", got.Message, "disk full")
	}
}

// TestReadStartupStatus_EOFIsOK pins the ErrStartupOK sentinel — this is
// the contract the parent uses to distinguish "child closed fd 3 quietly
// = success" from any other state.
func TestReadStartupStatus_EOFIsOK(t *testing.T) {
	got, err := ReadStartupStatus(strings.NewReader(""))
	if !errors.Is(err, ErrStartupOK) {
		t.Fatalf("err = %v, want ErrStartupOK", err)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

// TestReadStartupStatus_RoundTrip writes a status, reads it back, and
// asserts the structured shape survives. Mirrors what the parent sees
// when the child writes via WriteStartupStatusOnFD3.
func TestReadStartupStatus_RoundTrip(t *testing.T) {
	want := StartupStatus{
		Kind: StartupKindMigration,
		Projects: []StartupMigrationProject{
			{ID: "id1", Dir: "/x/y", Messages: []string{"m1"}},
		},
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(want); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ReadStartupStatus(&buf)
	if err != nil {
		t.Fatalf("ReadStartupStatus: %v", err)
	}
	if got.Kind != want.Kind || got.Projects[0].ID != "id1" || got.Projects[0].Messages[0] != "m1" {
		t.Fatalf("got = %+v, want = %+v", got, want)
	}
}

// TestReadStartupStatus_GarbageReturnsError makes sure malformed JSON
// (not EOF) is surfaced as a decode error, so callers can distinguish it
// from the success sentinel.
func TestReadStartupStatus_GarbageReturnsError(t *testing.T) {
	got, err := ReadStartupStatus(strings.NewReader("not json"))
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	if errors.Is(err, ErrStartupOK) {
		t.Fatalf("expected non-OK error, got ErrStartupOK")
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

// TestReadStartupStatus_PipeEOF mirrors the parent's real-world setup: an
// os.Pipe whose write-end is closed without writing should surface as
// ErrStartupOK on the read-end.
func TestReadStartupStatus_PipeEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	w.Close() // immediate close → read-end sees EOF

	got, err := ReadStartupStatus(r)
	if !errors.Is(err, ErrStartupOK) {
		t.Fatalf("err = %v, want ErrStartupOK", err)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

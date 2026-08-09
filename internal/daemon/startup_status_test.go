package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// The fd-3 tests below run the code under test in a SUBPROCESS, with a real
// pipe wired at descriptor 3 through ProcAttr.Files — the exact shape Spawn
// produces in production (see Spawn's own "Files index = fd number in the
// child" comment).
//
// They used to do it in-process, with a fd3Guard helper that did
// `syscall.Dup(3)` then `syscall.Dup3(pipe, 3, 0)` to install a pipe at
// descriptor 3 and put the original back afterwards. That was the cause of
// the long-standing "Unit tests" CI flake, and of the same crash locally at
// roughly 1 run in 4:
//
//	runtime: epollwait on fd 3 failed with 9
//	fatal error: runtime: netpoll failed
//
// In a Go test binary descriptor 3 is normally the RUNTIME'S OWN epoll
// instance. Dup3 atomically closes whatever occupies the target descriptor
// before installing the new one, so the guard destroyed the scheduler's
// netpoll descriptor for the whole window until it restored it. Any
// goroutine scheduling or timer tick inside that window called epollwait on
// a descriptor that was now a pipe (EINVAL, errno 22) or, after
// CloseStartupFD3 ran, closed (EBADF, errno 9) — both errnos were observed
// in CI. Nothing about the production code was wrong; the test's fd
// manipulation was.
//
// A subprocess has no such problem: the child gets descriptor 3 wired by the
// parent before it starts, exactly like a Spawn'd daemon, and its runtime
// puts its epoll instance somewhere else.

// fd3HelperEnvKey selects which helper the re-executed test binary should
// run. Empty (the normal case) makes TestFD3HelperProcess skip itself.
const fd3HelperEnvKey = "BOID_TEST_FD3_MODE"

// Exit codes the helper reports fd 3's final state with. Deliberately not
// 0/1, so an ordinary test-binary success or failure cannot be mistaken for
// a verdict.
const (
	fd3ExitStillOpen = 3
	fd3ExitClosed    = 4
)

// TestFD3HelperProcess is not a test. It is the child half of the fd-3 tests
// below: re-executed with fd3HelperEnvKey set and a pipe on descriptor 3, it
// runs one status-pipe helper and reports whether descriptor 3 survived via
// its exit code.
func TestFD3HelperProcess(t *testing.T) {
	mode := os.Getenv(fd3HelperEnvKey)
	if mode == "" {
		t.Skip("helper process; only runs when re-executed by an fd-3 test")
	}

	switch mode {
	case "close":
		CloseStartupFD3()
	case "write":
		WriteStartupStatusOnFD3(errors.New("boom"))
	default:
		fmt.Fprintf(os.Stderr, "unknown %s=%q\n", fd3HelperEnvKey, mode)
		os.Exit(1)
	}

	var st syscall.Stat_t
	if syscall.Fstat(statusPipeFD, &st) == nil {
		os.Exit(fd3ExitStillOpen)
	}
	os.Exit(fd3ExitClosed)
}

// runFD3Helper re-executes this test binary running only
// TestFD3HelperProcess, with a fresh pipe on descriptor 3.
//
// It returns whether the child left descriptor 3 open, plus whatever the
// child wrote into the pipe. spawnEnvSet mirrors what Spawn declares: true
// means "fd 3 really is the status pipe".
func runFD3Helper(t *testing.T, mode string, spawnEnvSet bool) (fd3StillOpen bool, written []byte) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestFD3HelperProcess$")
	// ExtraFiles[0] is the child's descriptor 3 — the same position
	// Spawn gives statusW in its ProcAttr.Files.
	cmd.ExtraFiles = []*os.File{w}
	env := append(os.Environ(), fd3HelperEnvKey+"="+mode)
	if spawnEnvSet {
		env = append(env, statusPipeEnvKey+"=1")
	} else {
		// os.Environ() may carry it in from an outer run; make the
		// "not launched by Spawn" case unambiguous.
		env = append(env, statusPipeEnvKey+"=")
	}
	cmd.Env = env

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		w.Close()
		t.Fatalf("start helper: %v", err)
	}
	// Drop the parent's copy so reading r sees EOF once the child is done.
	w.Close()

	payload, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("read status pipe: %v", readErr)
	}

	err = cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("wait helper: %v (stderr: %s)", err, stderr.String())
	}

	switch code {
	case fd3ExitStillOpen:
		return true, payload
	case fd3ExitClosed:
		return false, payload
	default:
		t.Fatalf("helper exited %d, want %d or %d (stderr: %s)",
			code, fd3ExitStillOpen, fd3ExitClosed, stderr.String())
		return false, nil
	}
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
	stillOpen, _ := runFD3Helper(t, "close", false)
	if !stillOpen {
		t.Fatal("CloseStartupFD3 closed fd 3 although the process was not launched by Spawn; an unrelated file (e.g. the SQLite DB) landing on fd 3 would be destroyed")
	}
}

// TestCloseStartupFD3_WithSpawnEnv_ClosesFD3 keeps the original contract
// intact for the Spawn path: the parent's read-end must observe EOF, which
// only happens if the child really closes its write-end.
func TestCloseStartupFD3_WithSpawnEnv_ClosesFD3(t *testing.T) {
	stillOpen, payload := runFD3Helper(t, "close", true)
	if stillOpen {
		t.Fatal("CloseStartupFD3 left fd 3 open under the Spawn env; the parent would block instead of seeing EOF")
	}
	if len(payload) != 0 {
		t.Errorf("status pipe carried %q; a plain close must signal success as EOF, with no payload", payload)
	}
}

// TestWriteStartupStatusOnFD3_WithoutSpawnEnv_LeavesFD3Open covers the
// failure-path twin of the above: the error reporter must not write into —
// or close — a descriptor that is not the status pipe.
func TestWriteStartupStatusOnFD3_WithoutSpawnEnv_LeavesFD3Open(t *testing.T) {
	stillOpen, payload := runFD3Helper(t, "write", false)
	if !stillOpen {
		t.Fatal("WriteStartupStatusOnFD3 closed fd 3 although the process was not launched by Spawn")
	}
	if len(payload) != 0 {
		t.Errorf("WriteStartupStatusOnFD3 wrote %q into a descriptor that is not the status pipe", payload)
	}
}

// TestWriteStartupStatusOnFD3_WithSpawnEnv_WritesPayload is new with the
// subprocess rewrite: the in-process version could not assert this, because
// reading the pipe needed the child to have exited first. It closes the loop
// on the happy path — under the Spawn env the error really does reach the
// parent as a decodable StartupStatus.
func TestWriteStartupStatusOnFD3_WithSpawnEnv_WritesPayload(t *testing.T) {
	stillOpen, payload := runFD3Helper(t, "write", true)
	if stillOpen {
		t.Error("WriteStartupStatusOnFD3 left fd 3 open; the parent would block waiting for EOF")
	}
	status, err := ReadStartupStatus(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadStartupStatus(%q): %v", payload, err)
	}
	if status.Kind != StartupKindOther || !strings.Contains(status.Message, "boom") {
		t.Errorf("status = %+v, want Kind=%s carrying \"boom\"", status, StartupKindOther)
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

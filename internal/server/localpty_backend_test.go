package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"golang.org/x/sys/unix"
)

// localPTYBackend is a real-PTY-backed backend.SandboxBackend, scoped to
// this package's test files only (server_phase3_test.go): it exercises the
// actual WS-attach / HTTP-resize wiring (Runner.Backend.Adopt ->
// SandboxSession.Subscribe/Resize) against a genuine kernel PTY, the same
// end-to-end depth the pre-PR-4 dispatcher.LocalRuntime gave those tests
// (docs/plans/volume-only-daemon.md §論点e removed LocalRuntime along with
// the rest of the userns backend) — without reintroducing userns/
// pivot_root/pasta, none of which these tests need: StartPTY spawns a live
// `bash -c` process attached to a fresh PTY and registers it for Adopt, so
// the client-facing attach/resize path drives a real terminal exactly like
// production dispatch does, just via a much simpler bootstrap than a full
// sandbox launch.
type localPTYBackend struct {
	mu       sync.Mutex
	sessions map[string]*localPTYSession
}

func newLocalPTYBackend() *localPTYBackend {
	return &localPTYBackend{sessions: make(map[string]*localPTYSession)}
}

var _ backend.SandboxBackend = (*localPTYBackend)(nil)

// StartPTY spawns `bash -c command` attached to a fresh PTY and registers
// the resulting session under a generated ID, returning that ID — the
// test's own bootstrap step (mirroring the pre-PR-4
// dispatcher.LocalRuntime.Start call these tests used to make directly).
func (b *localPTYBackend) StartPTY(command string) (id string, err error) {
	master, slave, err := openTestPTY()
	if err != nil {
		return "", fmt.Errorf("open pty: %w", err)
	}
	if err := setTestPTYSize(master, 24, 80); err != nil {
		master.Close()
		slave.Close()
		return "", fmt.Errorf("set default pty size: %w", err)
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = append(os.Environ(), "TERM=dumb")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return "", fmt.Errorf("start process: %w", err)
	}
	if err := slave.Close(); err != nil {
		master.Close()
		return "", fmt.Errorf("close slave pty: %w", err)
	}

	id = fmt.Sprintf("pty-%d", time.Now().UnixNano())
	sess := &localPTYSession{
		id:          id,
		cmd:         cmd,
		master:      master,
		subscribers: make(map[int]chan []byte),
		running:     true,
		done:        make(chan struct{}),
		readDone:    make(chan struct{}),
	}
	b.mu.Lock()
	b.sessions[id] = sess
	b.mu.Unlock()

	go sess.readLoop()
	go sess.waitLoop()

	return id, nil
}

func (b *localPTYBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, fmt.Errorf("localPTYBackend.Launch is not implemented — this test-only fake is driven via StartPTY")
}

func (b *localPTYBackend) Adopt(_ context.Context, runtimeID string) (backend.SandboxSession, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[runtimeID]
	return sess, ok
}

// RunWorkspaceInit satisfies dispatcher.WorkspaceInitExecutor — required of
// every backend since PR5; see runWorkspaceInitStub.
func (b *localPTYBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	return runWorkspaceInitStub(ctx, req)
}

func (b *localPTYBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// localPTYSession is a minimal, real-PTY-backed backend.SandboxSession —
// the pub-sub subset of the pre-PR-4 dispatcher.LocalRuntime's
// localRuntimeSession this package's attach/resize tests actually exercise
// (transcript accumulation + live subscriber fan-out + real TIOCSWINSZ
// resize), trimmed of everything specific to the removed userns
// runner-outer/-inner/-inner-child launch chain (spec files, runner-state
// diagnostics, stdin-forwarding pipes for non-interactive sessions, ...).
type localPTYSession struct {
	id     string
	cmd    *exec.Cmd
	master *os.File

	mu          sync.Mutex
	writerMu    sync.Mutex
	transcript  bytes.Buffer
	subscribers map[int]chan []byte
	nextSubID   int
	running     bool
	exit        backend.RuntimeExit

	done     chan struct{}
	readDone chan struct{}
}

var _ backend.SandboxSession = (*localPTYSession)(nil)

func (s *localPTYSession) ID() string { return s.id }

func (s *localPTYSession) Subscribe() (snapshot []byte, ch <-chan []byte, cancel func(), ok bool) {
	s.mu.Lock()
	raw := append([]byte(nil), s.transcript.Bytes()...)
	running := s.running
	var subID int
	var subCh chan []byte
	if running {
		subID = s.nextSubID
		s.nextSubID++
		subCh = make(chan []byte, 64)
		s.subscribers[subID] = subCh
	}
	s.mu.Unlock()

	if !running {
		return raw, nil, func() {}, false
	}
	return raw, subCh, func() { s.unsubscribe(subID) }, true
}

func (s *localPTYSession) unsubscribe(subID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subscribers[subID]; ok {
		close(ch)
		delete(s.subscribers, subID)
	}
}

func (s *localPTYSession) WriteInput(data []byte) error {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	_, err := s.master.Write(data)
	return err
}

func (s *localPTYSession) CloseInput() error { return nil }

func (s *localPTYSession) Resize(size backend.TerminalSize) error {
	if size.Rows <= 0 || size.Cols <= 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(s.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(size.Rows),
		Col: uint16(size.Cols),
	})
}

func (s *localPTYSession) Wait(ctx context.Context) (backend.RuntimeExit, error) {
	select {
	case <-ctx.Done():
		return backend.RuntimeExit{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exit, nil
	}
}

func (s *localPTYSession) Stop(context.Context) error {
	if s.cmd.Process != nil {
		_ = unix.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

func (s *localPTYSession) Signal(_ context.Context, sig syscall.Signal) error {
	if s.cmd.Process == nil {
		return nil
	}
	return unix.Kill(-s.cmd.Process.Pid, sig)
}

func (s *localPTYSession) readLoop() {
	defer close(s.readDone)
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.appendTranscript(chunk)
		}
		if err != nil {
			return
		}
	}
}

func (s *localPTYSession) appendTranscript(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transcript.Write(chunk)
	for id, ch := range s.subscribers {
		cp := append([]byte(nil), chunk...)
		select {
		case ch <- cp:
		default:
			close(ch)
			delete(s.subscribers, id)
		}
	}
}

func (s *localPTYSession) waitLoop() {
	err := s.cmd.Wait()
	<-s.readDone

	s.mu.Lock()
	s.running = false
	s.exit = backend.RuntimeExit{ExitCode: testExitCode(err)}
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
	s.mu.Unlock()

	_ = s.master.Close()
	close(s.done)
}

func testExitCode(err error) int {
	if err == nil {
		return 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return 1
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return 1
	}
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

// openTestPTY / setTestPTYSize are the minimal /dev/ptmx-based PTY
// primitives localPTYBackend.StartPTY needs — the same syscalls the
// removed dispatcher.LocalRuntime.Start used (runtime_local_linux.go,
// deleted in PR-4), kept here as a test-local, backend-agnostic helper
// rather than reintroduced as production dispatcher code.
func openTestPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	ptyNumber, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", ptyNumber)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setTestPTYSize(master *os.File, rows, cols int) error {
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

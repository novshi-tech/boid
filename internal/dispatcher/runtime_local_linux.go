//go:build linux

package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const localRuntimeTranscriptFile = "transcript.log"

type LocalRuntime struct {
	RootDir string

	mu       sync.Mutex
	sessions map[string]*localRuntimeSession
}

type localRuntimeSession struct {
	id             string
	cmd            *exec.Cmd
	master         *os.File
	transcriptFile *os.File

	mu          sync.Mutex
	writerMu    sync.Mutex // protects concurrent writes to master
	transcript  bytes.Buffer
	subscribers map[int]chan []byte
	nextSubID   int
	running     bool
	exit        RuntimeExit

	done     chan struct{}
	readDone chan struct{}
}

func (r *LocalRuntime) Start(_ context.Context, spec RuntimeStartSpec) (*RuntimeHandle, error) {
	if r.RootDir == "" {
		return nil, fmt.Errorf("local runtime root directory is required")
	}
	if spec.Command == "" {
		return nil, fmt.Errorf("runtime command is required")
	}

	runtimeID := uuid.NewString()
	runtimeDir := filepath.Join(r.RootDir, runtimeID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir runtime dir: %w", err)
	}

	transcriptPath := filepath.Join(runtimeDir, localRuntimeTranscriptFile)
	transcriptFile, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open transcript file: %w", err)
	}

	master, slave, err := openPTY()
	if err != nil {
		transcriptFile.Close()
		return nil, fmt.Errorf("open pty: %w", err)
	}
	if err := setPTYSize(master, TerminalSize{Cols: 80, Rows: 24}); err != nil {
		master.Close()
		slave.Close()
		transcriptFile.Close()
		return nil, fmt.Errorf("set default pty size: %w", err)
	}

	cmd := exec.Command("bash", "-lc", spec.Command)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		transcriptFile.Close()
		return nil, fmt.Errorf("start process: %w", err)
	}
	if err := slave.Close(); err != nil {
		master.Close()
		transcriptFile.Close()
		return nil, fmt.Errorf("close slave pty: %w", err)
	}

	session := &localRuntimeSession{
		id:             runtimeID,
		cmd:            cmd,
		master:         master,
		transcriptFile: transcriptFile,
		subscribers:    make(map[int]chan []byte),
		running:        true,
		done:           make(chan struct{}),
		readDone:       make(chan struct{}),
	}

	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = make(map[string]*localRuntimeSession)
	}
	r.sessions[runtimeID] = session
	r.mu.Unlock()

	go session.readLoop()
	go session.waitLoop()

	return &RuntimeHandle{
		ID:          runtimeID,
		Interactive: spec.Interactive,
		TTY:         spec.TTY,
	}, nil
}

func (r *LocalRuntime) Attach(ctx context.Context, runtimeID string, req RuntimeAttachRequest) error {
	session, err := r.session(runtimeID)
	if err != nil {
		return err
	}

	output := req.Output
	if output == nil {
		output = io.Discard
	}

	snapshot, subID, subCh, running := session.subscribe()
	if len(snapshot) > 0 {
		if _, err := output.Write(snapshot); err != nil {
			if errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
	}
	if !running {
		return nil
	}
	defer session.unsubscribe(subID)

	errCh := make(chan error, 1)
	if req.Input != nil {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, readErr := req.Input.Read(buf)
				if n > 0 {
					if writeErr := session.writeMaster(buf[:n]); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
						errCh <- writeErr
						return
					}
				}
				if readErr != nil {
					if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, os.ErrClosed) {
						errCh <- readErr
						return
					}
					break
				}
			}
			errCh <- nil
		}()
	} else {
		close(errCh)
	}

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case err := <-errCh:
			if err != nil {
				return err
			}
			errCh = nil
		case chunk, ok := <-subCh:
			if !ok {
				return nil
			}
			if len(chunk) == 0 {
				continue
			}
			if _, err := output.Write(chunk); err != nil {
				if errors.Is(err, os.ErrClosed) {
					return nil
				}
				return err
			}
		}
	}
}

func (r *LocalRuntime) Resize(_ context.Context, runtimeID string, size TerminalSize) error {
	session, err := r.session(runtimeID)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.running {
		return nil
	}
	return setPTYSize(session.master, size)
}

func (r *LocalRuntime) Wait(ctx context.Context, runtimeID string) (RuntimeExit, error) {
	session, err := r.session(runtimeID)
	if err != nil {
		return RuntimeExit{}, err
	}

	select {
	case <-ctx.Done():
		return RuntimeExit{}, ctx.Err()
	case <-session.done:
		return session.exitStatus(), nil
	}
}

func (r *LocalRuntime) Stop(ctx context.Context, runtimeID string) error {
	session, err := r.session(runtimeID)
	if err != nil {
		return err
	}

	if !session.isRunning() {
		return nil
	}
	if err := terminateProcessGroup(session.cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	select {
	case <-session.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}

	if err := terminateProcessGroup(session.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}

	select {
	case <-session.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WriteInputRuntime writes data to the PTY master of the given runtime.
// Returns nil if the session is not running or has already exited.
func (r *LocalRuntime) WriteInputRuntime(runtimeID string, data []byte) error {
	session, err := r.session(runtimeID)
	if err != nil {
		return nil
	}
	session.mu.Lock()
	running := session.running
	session.mu.Unlock()
	if !running {
		return nil
	}
	if err := session.writeMaster(data); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

func (r *LocalRuntime) SupportsAttach(runtimeID string) bool {
	_, err := r.session(runtimeID)
	return err == nil
}

func (r *LocalRuntime) session(runtimeID string) (*localRuntimeSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.sessions == nil {
		return nil, fmt.Errorf("runtime %s not found", runtimeID)
	}
	session, ok := r.sessions[runtimeID]
	if !ok {
		return nil, fmt.Errorf("runtime %s not found", runtimeID)
	}
	return session, nil
}

func (s *localRuntimeSession) readLoop() {
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

func (s *localRuntimeSession) waitLoop() {
	err := s.cmd.Wait()
	<-s.readDone

	s.mu.Lock()
	s.running = false
	s.exit = RuntimeExit{ExitCode: exitCode(err)}
	s.closeSubscribersLocked()
	s.mu.Unlock()

	_ = s.master.Close()
	_ = s.transcriptFile.Close()
	close(s.done)
}

func (s *localRuntimeSession) appendTranscript(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.transcript.Write(chunk)
	_, _ = s.transcriptFile.Write(chunk)

	for id, ch := range s.subscribers {
		copyChunk := append([]byte(nil), chunk...)
		select {
		case ch <- copyChunk:
		default:
			close(ch)
			delete(s.subscribers, id)
		}
	}
}

func (s *localRuntimeSession) subscribe() ([]byte, int, chan []byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapshot := append([]byte(nil), s.transcript.Bytes()...)
	if !s.running {
		return snapshot, 0, nil, false
	}

	subID := s.nextSubID
	s.nextSubID++
	ch := make(chan []byte, 64)
	s.subscribers[subID] = ch
	return snapshot, subID, ch, true
}

func (s *localRuntimeSession) unsubscribe(subID int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch, ok := s.subscribers[subID]
	if ok {
		close(ch)
		delete(s.subscribers, subID)
	}
}

func (s *localRuntimeSession) closeSubscribersLocked() {
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
}

func (s *localRuntimeSession) writeMaster(data []byte) error {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	_, err := s.master.Write(data)
	return err
}

func (s *localRuntimeSession) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *localRuntimeSession) exitStatus() RuntimeExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}

func openPTY() (*os.File, *os.File, error) {
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

func setPTYSize(master *os.File, size TerminalSize) error {
	if master == nil {
		return fmt.Errorf("pty master is required")
	}
	if size.Rows <= 0 || size.Cols <= 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(size.Rows),
		Col: uint16(size.Cols),
	})
}

func terminateProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	return unix.Kill(-pid, sig)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	return 1
}

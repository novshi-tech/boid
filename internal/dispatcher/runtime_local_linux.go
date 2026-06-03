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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const localRuntimeTranscriptFile = "transcript.log"

// maxSnapshotScrollback bounds how many scrolled-off lines a connect-time
// snapshot prepends ahead of the visible screen, so the web client can scroll
// back through history without an unbounded payload on long sessions. Keep
// aligned with the xterm scrollback in web/static/boid-terminal.js.
const maxSnapshotScrollback = 2000

type LocalRuntime struct {
	RootDir string

	mu       sync.Mutex
	sessions map[string]*localRuntimeSession
}

type localRuntimeSession struct {
	id             string
	cmd            *exec.Cmd
	master         *os.File // PTY master (interactive) or pipe read-end (non-interactive)
	interactive    bool
	transcriptFile *os.File
	transcriptPath string

	mu          sync.Mutex
	writerMu    sync.Mutex // protects concurrent writes to master
	transcript  bytes.Buffer
	cols, rows  int // current PTY size = the width the transcript is recorded at
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

	runtimeID := spec.DesiredID
	if runtimeID == "" {
		runtimeID = uuid.NewString()
	}
	runtimeDir := filepath.Join(r.RootDir, runtimeID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir runtime dir: %w", err)
	}

	transcriptPath := filepath.Join(runtimeDir, localRuntimeTranscriptFile)
	transcriptFile, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open transcript file: %w", err)
	}

	var reader *os.File
	var cmd *exec.Cmd

	if spec.Interactive {
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

		cmd = exec.Command("bash", "-lc", spec.Command)
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
		reader = master
	} else {
		pr, pw, err := os.Pipe()
		if err != nil {
			transcriptFile.Close()
			return nil, fmt.Errorf("open pipe: %w", err)
		}

		cmd = exec.Command("bash", "-lc", spec.Command)
		cmd.Stdout = pw
		cmd.Stderr = pw
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid: true,
		}

		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			transcriptFile.Close()
			return nil, fmt.Errorf("start process: %w", err)
		}
		if err := pw.Close(); err != nil {
			pr.Close()
			transcriptFile.Close()
			return nil, fmt.Errorf("close pipe write end: %w", err)
		}
		reader = pr
	}

	session := &localRuntimeSession{
		id:             runtimeID,
		cmd:            cmd,
		master:         reader,
		interactive:    spec.Interactive,
		transcriptFile: transcriptFile,
		transcriptPath: transcriptPath,
		cols:           80, // matches the default PTY size set above (setPTYSize 80x24)
		rows:           24,
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
	if !session.running || !session.interactive {
		return nil
	}
	if err := setPTYSize(session.master, size); err != nil {
		return err
	}
	// Track the new size so a later subscribe() rebuilds the grid at the width
	// the transcript is actually being recorded at.
	if size.Cols > 0 && size.Rows > 0 {
		session.cols = size.Cols
		session.rows = size.Rows
	}
	return nil
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

// Signal delivers a single signal to the runtime's process group without any
// SIGKILL follow-up. NotifyTask uses this for SIGUSR1 (agent-stop) — the
// signal is delivered process-group-wide (kill(-pgid, sig)) and processes
// configured to ignore it via `trap '' USR1` / SIG_IGN survive unaffected.
// No-op when the runtime session has already exited.
func (r *LocalRuntime) Signal(_ context.Context, runtimeID string, sig syscall.Signal) error {
	session, err := r.session(runtimeID)
	if err != nil {
		return err
	}
	if !session.isRunning() {
		return nil
	}
	if err := terminateProcessGroup(session.cmd.Process.Pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
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
	// Capture the recorded bytes, the recording width, and register the live
	// channel atomically under the lock so the snapshot and the subsequent
	// deltas share an exact boundary: the snapshot resolves bytes[:N] and every
	// chunk delivered on the channel is bytes[N:]. The client paints the
	// snapshot, then applies the raw deltas on top.
	s.mu.Lock()
	raw := append([]byte(nil), s.transcript.Bytes()...)
	cols, rows := s.cols, s.rows
	interactive := s.interactive
	running := s.running

	var subID int
	var ch chan []byte
	if running {
		subID = s.nextSubID
		s.nextSubID++
		ch = make(chan []byte, 64)
		s.subscribers[subID] = ch
	}
	s.mu.Unlock()

	// Interactive sessions carry a TUI whose output is a stream of width-
	// dependent relative cursor moves. Replaying the raw history at a different
	// width accumulates into garbage, so resolve it to the current screen grid
	// (done outside the lock — it is CPU-bound and must not block live
	// broadcast). Non-interactive sessions are plain log streams replayed
	// verbatim. See docs/plans/web-terminal-vt-emulator.md.
	snapshot := raw
	if interactive {
		snapshot = renderTerminalSnapshot(raw, cols, rows)
	}
	return snapshot, subID, ch, running
}

// renderTerminalSnapshot feeds the recorded PTY bytes through a virtual
// terminal emulator sized to the recording width and returns the resolved
// screen as a width-independent ANSI dump (SGR styles preserved). The client
// paints this onto a cleared terminal and its xterm reflows it to the client's
// own width — far cleaner than replaying the raw transcript, where relative
// cursor moves recorded at one width corrupt at another.
func renderTerminalSnapshot(raw []byte, cols, rows int) []byte {
	if len(raw) == 0 {
		return nil
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}

	emu := vt.NewEmulator(cols, rows)

	// The emulator answers device queries (DA1, DSR, XTVERSION, ...) embedded in
	// the recorded output by writing replies to its synchronous input pipe.
	// Nobody consumes those replies here — the real PTY already answered the
	// queries — but the pipe write blocks until drained, so emu.Write() would
	// deadlock without a concurrent reader.
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, emu)
		close(drained)
	}()

	_, _ = emu.Write(raw)

	// Stop the drain reader by closing only the reply pipe's write end, which
	// hands the reader a clean EOF. We deliberately do NOT call emu.Close() to
	// stop it: emu.Close flips an internal `closed` flag that the reader checks
	// on every emu.Read, and the race detector (correctly) flags that as a data
	// race in x/vt. Once <-drained confirms the reader is gone, flipping that
	// flag has no concurrent observer.
	if pw, ok := emu.InputPipe().(*io.PipeWriter); ok {
		_ = pw.Close()
		<-drained
		_ = emu.Close()
	} else {
		// Defensive fallback if x/vt ever changes the pipe type: emu.Close
		// still unblocks the reader (with the data race noted above).
		_ = emu.Close()
		<-drained
	}

	// Prepend the lines that have scrolled off the top (the emulator collects
	// them in its scrollback) so the client can scroll back through history,
	// then the current visible screen. Without this the dump is only the
	// viewport and all earlier output is lost on (re)connect. Cap to the most
	// recent maxSnapshotScrollback lines to bound the payload on long sessions;
	// keep it aligned with the xterm scrollback in web/static/boid-terminal.js.
	var b strings.Builder
	if sb := emu.Scrollback(); sb != nil {
		lines := sb.Lines()
		if start := len(lines) - maxSnapshotScrollback; start > 0 {
			lines = lines[start:]
		}
		for _, ln := range lines {
			b.WriteString(ln.Render())
			b.WriteByte('\n')
		}
	}
	b.WriteString(emu.Render())

	// Buffer.Render joins rows with a bare LF (and we used LF above). A raw-mode
	// xterm treats LF as line-feed-only (no carriage return), which would
	// stagger the output into a staircase, so emit CRLF to anchor each row at
	// column 0.
	return []byte(strings.ReplaceAll(b.String(), "\n", "\r\n"))
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
	if !s.interactive {
		return nil
	}
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
	exit := s.exit
	exit.TranscriptPath = s.transcriptPath
	return exit
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

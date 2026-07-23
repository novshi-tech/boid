// Package daemon provides helpers for daemonizing the boid server process.
// It implements a self-re-exec pattern: the parent spawns a copy of itself
// with BOID_DAEMON_CHILD=1, waits for the UNIX socket to become ready, and
// then exits.  The child redirects stdin/stdout/stderr to a log file, calls
// syscall.Setsid to detach from the terminal session, and runs the server.
package daemon

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/logrotate"
)

const daemonEnvKey = "BOID_DAEMON_CHILD"

// IsChild reports whether the current process is the daemon child.
func IsChild() bool {
	return os.Getenv(daemonEnvKey) == "1"
}

// logStdoutEnvKey opts the daemon child out of two pieces of host-daemon
// double-fork machinery that only make sense when `boid start` is
// detaching itself from a real controlling terminal with no other
// supervisor involved — neither applies inside a container (PR9, docs/
// plans/phase6-container-backend.md §PR6's compose.yml own "Known
// limitations" note: "A real docker logs-visible foreground mode... is a
// nice-to-have for a later PR"):
//
//   - RedirectToLogRotating's self-pipe dup2(stdout/stderr) dance: a
//     container runtime's own log driver (dockerd, under compose) already
//     captures stdout/stderr — no separate log-file redirect is needed,
//     and empirically this dance does not survive this container's PID1
//     (docker-init/tini) setup cleanly (see below).
//   - syscall.Setsid(): fails outright with EPERM when the calling
//     process is ALREADY its own process group leader — which a
//     container's entrypoint process (tini's direct child) commonly is.
//     This is docs/plans/phase6-cutover-followups.md's actual root cause
//     for the e2e-container job's startup crash: the pre-BOID_LOG_STDOUT
//     symptom (SIGPIPE, exit 141, zero visible docker-logs output) turned
//     out to be this same "setsid: operation not permitted" error being
//     written into the now-redirected — and shortly abandoned — log
//     pipe, not a bug in the redirect mechanism itself. Detaching from a
//     controlling terminal is meaningless in a container regardless (there
//     is none to detach from), so skipping the call entirely is correct,
//     not just a workaround.
//
// Set by build/container/compose.yml's daemon service. False (every
// pre-PR9 caller) preserves exactly today's RedirectToLogRotating +
// Setsid behavior.
const logStdoutEnvKey = "BOID_LOG_STDOUT"

// ShouldLogToStdout reports whether the daemon child should skip
// RedirectToLogRotating (let stdout/stderr flow to whatever already
// captures them) and syscall.Setsid (meaningless, and EPERM-failing, when
// already a process group leader) — see logStdoutEnvKey's doc comment for
// the full rationale for bundling both behind one flag.
func ShouldLogToStdout() bool {
	return os.Getenv(logStdoutEnvKey) == "1"
}

// LogFilePath returns the path for the daemon log file.
// Uses $XDG_STATE_HOME/boid/boid.log, falling back to ~/.local/state/boid/boid.log.
func LogFilePath() string {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateDir, "boid", "boid.log")
}

// IsSocketAlive reports whether something is actively listening on socketPath.
// It returns true if a UNIX domain socket can be dialed within timeout, which
// distinguishes a running server from a stale socket file (ECONNREFUSED) or
// missing socket file (ENOENT).
func IsSocketAlive(socketPath string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// RedirectToLog opens logPath (O_APPEND|O_CREATE|O_WRONLY, 0o644), creates the
// parent directory if necessary, and replaces file descriptors 0, 1, and 2:
//   - fd 0 (stdin)  → /dev/null
//   - fd 1 (stdout) → logPath
//   - fd 2 (stderr) → logPath
func RedirectToLog(logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logF.Close()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()

	if err := syscall.Dup2(int(devNull.Fd()), 0); err != nil {
		return fmt.Errorf("redirect stdin: %w", err)
	}
	if err := syscall.Dup2(int(logF.Fd()), 1); err != nil {
		return fmt.Errorf("redirect stdout: %w", err)
	}
	if err := syscall.Dup2(int(logF.Fd()), 2); err != nil {
		return fmt.Errorf("redirect stderr: %w", err)
	}
	return nil
}

// RedirectToLogRotating is the size-rotating variant of RedirectToLog.
// It creates an OS pipe, redirects stdin to /dev/null, and redirects
// stdout and stderr to the pipe write-end.  A background goroutine copies
// from the pipe read-end into a logrotate.Writer so the log is rotated
// automatically when it grows past MaxSize.
//
// The goroutine exits (and closes the writer) when all write-ends of the
// pipe are closed, which happens naturally when the process exits.
func RedirectToLogRotating(logPath string) error {
	w := &logrotate.Writer{Path: logPath}

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create log pipe: %w", err)
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()

	if err := syscall.Dup2(int(devNull.Fd()), 0); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("redirect stdin: %w", err)
	}
	if err := syscall.Dup2(int(pw.Fd()), 1); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("redirect stdout: %w", err)
	}
	if err := syscall.Dup2(int(pw.Fd()), 2); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("redirect stderr: %w", err)
	}
	// The dup'd descriptors (fd 1, fd 2) keep the write-end alive.
	pw.Close()

	go func() {
		defer pr.Close()
		defer w.Close()
		io.Copy(w, pr) //nolint:errcheck
	}()

	return nil
}

// Spawn forks a daemon child by re-executing the current binary with the same
// arguments and with BOID_DAEMON_CHILD=1 added to the environment.
//
// It also wires a one-shot status pipe on the child's fd 3 so the child can
// surface its startup outcome to the parent without depending on log file
// scraping. The parent receives the read-end (statusR). EOF on the read-end
// means the child closed fd 3 without writing — which the child does on
// successful startup (see RedirectToLogRotating callers in cmd/start.go).
// A JSON payload on the read-end means startup failed; the parent decodes
// it (see internal/daemon/startup_status.go) to drive auto-migration or
// surface the cause.
//
// Spawn closes its own copy of the write-end before returning so that EOF
// arrives at statusR even if the child exits without explicitly closing
// fd 3 (e.g. crash). The caller is responsible for closing statusR.
func Spawn(args []string) (int, *os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, nil, fmt.Errorf("resolve executable: %w", err)
	}

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 0, nil, fmt.Errorf("create status pipe: %w", err)
	}

	env := append(os.Environ(), daemonEnvKey+"=1")

	// Files index = fd number in the child. fds 0/1/2 inherit from the
	// parent (RedirectToLogRotating swaps them to the log pipe before the
	// daemon writes anything). fd 3 is the status pipe write-end.
	proc, err := os.StartProcess(exe, args, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr, statusW},
	})
	if err != nil {
		statusR.Close()
		statusW.Close()
		return 0, nil, fmt.Errorf("start daemon process: %w", err)
	}
	pid := proc.Pid
	if err := proc.Release(); err != nil {
		statusR.Close()
		statusW.Close()
		return 0, nil, fmt.Errorf("release daemon process: %w", err)
	}
	// Close the parent's copy of the write-end so that EOF arrives at the
	// read-end when the child exits without writing (or after the child
	// itself closes fd 3 on success).
	statusW.Close()
	return pid, statusR, nil
}

// WaitForSocket polls socketPath using net.Dial until a connection succeeds or
// timeout elapses.  It returns nil on success, or a descriptive error on timeout.
func WaitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", socketPath)
}

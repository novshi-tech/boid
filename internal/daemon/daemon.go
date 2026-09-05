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
// detaching from a real controlling terminal with no other supervisor
// involved — neither applies inside a container, where a runtime's own
// log driver already captures stdout/stderr and the entrypoint process is
// already its own process group leader (making Setsid fail with EPERM).
// Set by build/container/compose.yml's daemon service; see
// docs/plans/phase6-container-backend.md for the full rationale.
const logStdoutEnvKey = "BOID_LOG_STDOUT"

// ShouldLogToStdout reports whether the daemon child should skip
// RedirectToLogRotating and syscall.Setsid — see logStdoutEnvKey's doc
// comment for why.
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
// scraping. The parent receives the read-end (statusR): EOF means the child
// closed fd 3 without writing (successful startup); a JSON payload (see
// startup_status.go) means startup failed.
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

	env := spawnEnv(os.Environ())

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

// spawnEnv builds the child's environment: BOID_DAEMON_CHILD marks it as the
// daemon process, and statusPipeEnvKey separately declares that fd 3 carries
// the status pipe. The two are deliberately distinct — supervisors that run
// `boid start` themselves (build/container/compose.yml, or --foreground) set
// the former but wire nothing onto fd 3, so only Spawn may set the latter.
func spawnEnv(base []string) []string {
	return append(append([]string(nil), base...), daemonEnvKey+"=1", statusPipeEnvKey+"=1")
}

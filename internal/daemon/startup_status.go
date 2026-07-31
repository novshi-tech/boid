package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// statusPipeFD is the file descriptor the parent wires for the status pipe
// when spawning the daemon child. See daemon.Spawn.
const statusPipeFD = 3

// statusPipeEnvKey is set by Spawn — and only by Spawn — to declare that fd 3
// really is the status pipe write-end it wired. Everything that touches fd 3
// by its hard-coded number must gate on this.
//
// BOID_DAEMON_CHILD is NOT usable for that check: build/container/compose.yml
// sets it too, and runs `boid start` directly (cmd/start.go's --foreground
// does the same), so runDaemonChild is reached with NOTHING attached to fd 3.
// In that shape fd 3 is simply the lowest free descriptor, and server.New's
// db.Open lands the SQLite database on it. Closing or writing to it then
// destroys an unrelated file: on 2026-07-31 the containerized daemon started
// cleanly, closed fd 3 as its "startup ok" signal, and every subsequent query
// failed with SQLITE_IOERR_FSTAT (1802) / SQLITE_CORRUPT (11) because the
// database's descriptor was gone (fstat(3) = EBADF) while its -wal/-shm
// descriptors stayed open.
const statusPipeEnvKey = "BOID_STARTUP_STATUS_PIPE"

// statusPipeFile returns fd 3 as an *os.File only when this process was
// launched by Spawn with the status pipe wired. Any other launch shape gets
// nil, leaving fd 3 untouched.
func statusPipeFile() *os.File {
	if os.Getenv(statusPipeEnvKey) != "1" {
		return nil
	}
	return os.NewFile(statusPipeFD, "boid-startup-status")
}

// StartupStatusKind discriminates the payload variants written on fd 3.
type StartupStatusKind string

const (
	// StartupKindMigration indicates one or more registered projects need
	// `boid project migrate` to load. The parent can offer auto-migration.
	StartupKindMigration StartupStatusKind = "migration"
	// StartupKindOther indicates a startup failure with no structured
	// remediation path. The parent prints Message and points the user at
	// the log file.
	StartupKindOther StartupStatusKind = "other"
)

// StartupMigrationProject mirrors orchestrator.ProjectMigrationIssue at the
// wire level. Keeping a separate type lets the daemon package own the
// pipe schema without leaking yaml-tag concerns from the orchestrator side.
type StartupMigrationProject struct {
	ID       string   `json:"id,omitempty"`
	Dir      string   `json:"dir"`
	Messages []string `json:"messages"`
}

// StartupStatus is the JSON payload the daemon child writes on fd 3 when
// startup fails. On success the child closes fd 3 without writing — the
// parent observes EOF (ReadStartupStatus returns ErrStartupOK).
type StartupStatus struct {
	Kind     StartupStatusKind         `json:"kind"`
	Message  string                    `json:"message,omitempty"`  // populated when Kind == other
	Projects []StartupMigrationProject `json:"projects,omitempty"` // populated when Kind == migration
}

// ErrStartupOK is returned by ReadStartupStatus when the child closed fd 3
// without writing a payload — i.e. the daemon started successfully.
var ErrStartupOK = errors.New("daemon startup succeeded")

// WriteStartupStatusOnFD3 inspects the given startup error and writes a
// structured StartupStatus to fd 3 if the child was launched via Spawn.
// It is best-effort: failures to open fd 3 or to write are intentionally
// swallowed because they only degrade the parent's UX, not the child's
// log output. Callers should still return the original error so the
// daemon's log (boid.log) records the cause.
//
// When err contains a wrapped *orchestrator.ProjectMigrationError (any
// depth), the status is recorded as Kind=migration with each issue
// serialised. Otherwise the status is Kind=other with the err.Error() text.
func WriteStartupStatusOnFD3(err error) {
	if err == nil {
		return
	}
	f := statusPipeFile()
	if f == nil {
		return
	}
	defer f.Close()

	status := buildStartupStatus(err)
	enc := json.NewEncoder(f)
	_ = enc.Encode(status)
}

// CloseStartupFD3 closes fd 3 without writing a payload. The parent
// observes EOF on its read-end and treats it as success. Best-effort, and a
// no-op unless Spawn actually wired the pipe (see statusPipeEnvKey).
func CloseStartupFD3() {
	f := statusPipeFile()
	if f == nil {
		return
	}
	_ = f.Close()
}

// ReadStartupStatus decodes a StartupStatus from r. EOF (the child closed
// fd 3 without writing) is returned as ErrStartupOK so callers can branch
// cleanly on success vs structured failure.
func ReadStartupStatus(r io.Reader) (*StartupStatus, error) {
	var s StartupStatus
	dec := json.NewDecoder(r)
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrStartupOK
		}
		return nil, fmt.Errorf("decode startup status: %w", err)
	}
	return &s, nil
}

// buildStartupStatus walks err's error chain looking for a typed
// *orchestrator.ProjectMigrationError; if found it serialises every issue
// onto a StartupStatus{Kind: migration}. Otherwise it returns
// StartupStatus{Kind: other, Message: err.Error()}.
func buildStartupStatus(err error) StartupStatus {
	var migErr *orchestrator.ProjectMigrationError
	if errors.As(err, &migErr) && len(migErr.Projects) > 0 {
		ps := make([]StartupMigrationProject, len(migErr.Projects))
		for i, p := range migErr.Projects {
			ps[i] = StartupMigrationProject{
				ID:       p.ProjectID,
				Dir:      p.Dir,
				Messages: append([]string(nil), p.Messages...),
			}
		}
		return StartupStatus{Kind: StartupKindMigration, Projects: ps}
	}
	return StartupStatus{Kind: StartupKindOther, Message: err.Error()}
}

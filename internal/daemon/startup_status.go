package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// statusPipeFD is the file descriptor the parent wires for the status pipe
// when spawning the daemon child. See daemon.Spawn.
const statusPipeFD = 3

// statusPipeEnvKey is set by Spawn — and only by Spawn — to declare that fd 3
// really is the status pipe write-end it wired. Everything that touches fd 3
// by its hard-coded number must gate on this: without it (e.g. when
// `boid start` is invoked directly with nothing wired to fd 3), fd 3 is
// just the lowest free descriptor and may belong to something else
// entirely (server.New's SQLite db.Open has landed there before) —
// closing or writing to it then corrupts that unrelated resource.
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
	// StartupKindOther indicates a startup failure with no structured
	// remediation path. The parent prints Message and points the user at
	// the log file.
	StartupKindOther StartupStatusKind = "other"
)

// StartupStatus is the JSON payload the daemon child writes on fd 3 when
// startup fails. On success the child closes fd 3 without writing — the
// parent observes EOF (ReadStartupStatus returns ErrStartupOK).
type StartupStatus struct {
	Kind    StartupStatusKind `json:"kind"`
	Message string            `json:"message,omitempty"` // populated when Kind == other
}

// ErrStartupOK is returned by ReadStartupStatus when the child closed fd 3
// without writing a payload — i.e. the daemon started successfully.
var ErrStartupOK = errors.New("daemon startup succeeded")

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

func buildStartupStatus(err error) StartupStatus {
	return StartupStatus{Kind: StartupKindOther, Message: err.Error()}
}

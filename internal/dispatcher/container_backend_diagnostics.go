package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// diagnosticsFileName is the fixed filename NewDefaultDiagnosticsCollector
// writes under <dir>/<containerID>/ — the sibling of
// localRuntimeTranscriptFile ("transcript.log") under the same per-job
// directory.
const diagnosticsFileName = "diagnostics.json"

// diagnosticsMaxLogBytes bounds how much of a container's docker-side log
// buffer (ContainerLogs) NewDefaultDiagnosticsCollector captures. This is a
// diagnostic artifact for post-hoc troubleshooting, not the full-persistence
// transcript spool — a generous but finite cap keeps a runaway or
// spam-logging container from writing an unbounded diagnostics.json.
const diagnosticsMaxLogBytes = 256 * 1024

// containerDiagnostics is the JSON shape NewDefaultDiagnosticsCollector
// writes to <dir>/<containerID>/diagnostics.json. Every field is
// best-effort: an inspect or logs-fetch failure degrades gracefully (see
// the collector's own doc comment) rather than skipping the whole file.
type containerDiagnostics struct {
	ContainerID string    `json:"container_id"`
	CapturedAt  time.Time `json:"captured_at"`
	ExitCode    int       `json:"exit_code"`
	// InspectExitCode / InspectStatus / OOMKilled / InspectError come from
	// ContainerInspect's State, captured separately from the Wait-reported
	// ExitCode above: they can disagree, and both are useful for diagnosis.
	InspectExitCode int    `json:"inspect_exit_code,omitempty"`
	InspectStatus   string `json:"inspect_status,omitempty"`
	OOMKilled       bool   `json:"oom_killed"`
	InspectError    string `json:"inspect_error,omitempty"`
	// CollectorError records a failure of the collector's OWN inspect/logs
	// calls, distinct from InspectError (dockerd's own State.Error field).
	CollectorError string `json:"collector_error,omitempty"`
	// DockerLogsTail is dockerd's own retained log buffer, independent of
	// this backend's attach-stream transcript spool — useful for the
	// OOM-killed / setup-failure case the transcript spool can miss.
	// Truncated to diagnosticsMaxLogBytes.
	DockerLogsTail string `json:"docker_logs_tail,omitempty"`
	// EngineError mirrors backend.RuntimeExit.EngineError verbatim: when the
	// container engine itself failed to report an exit status — as opposed
	// to the job's own command failing — this is the only surviving
	// description of what went wrong. Empty when this exit was not an
	// engine fault.
	EngineError string `json:"engine_error,omitempty"`
}

// NewDefaultDiagnosticsCollector returns a ContainerBackendOptions.
// DiagnosticsCollector implementation suitable for production wiring.
//
// The returned collector runs only when exit.ExitCode != 0 (a clean exit
// already has its full output durable via the transcript spool, so it skips
// the extra ContainerInspect/ContainerLogs round trip; an engine-fault exit
// is always != 0, so this scope covers that case too). On an abnormal exit
// it captures ContainerInspect (exit code / status / OOMKilled / dockerd's
// own State.Error), a bounded tail of ContainerLogs (dockerd's own
// independently-retained log buffer, which can catch what a SIGKILL'd
// container's attach-stream transcript spool misses), and exit.EngineError
// verbatim — writing all of it to <dir>/<containerID>/diagnostics.json, the
// sibling of transcript.log under the same per-job directory. Every step is
// best-effort: an inspect or logs failure is recorded in the diagnostics.json
// itself (CollectorError) rather than losing the whole artifact, and the
// collector never returns an error to its caller (this runs strictly before
// ContainerRemove, so it must not block or fail container teardown).
//
// dir empty makes the returned collector a no-op — there is no directory to
// write into (mirrors openTranscriptSpool's identical empty-dir degrade).
func NewDefaultDiagnosticsCollector(api dockerAPI, dir string) func(ctx context.Context, containerID string, exit backend.RuntimeExit) {
	return func(ctx context.Context, containerID string, exit backend.RuntimeExit) {
		if dir == "" || exit.ExitCode == 0 {
			return
		}
		diag := containerDiagnostics{
			ContainerID: containerID,
			CapturedAt:  time.Now().UTC(),
			ExitCode:    exit.ExitCode,
			EngineError: exit.EngineError,
		}

		insp, err := api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		switch {
		case err != nil:
			diag.CollectorError = "inspect: " + err.Error()
		case insp.Container.State != nil:
			diag.InspectExitCode = insp.Container.State.ExitCode
			diag.InspectStatus = string(insp.Container.State.Status)
			diag.OOMKilled = insp.Container.State.OOMKilled
			diag.InspectError = insp.Container.State.Error
		}

		if tail, logErr := captureContainerLogsTail(ctx, api, containerID); logErr != nil {
			if diag.CollectorError != "" {
				diag.CollectorError += "; "
			}
			diag.CollectorError += "logs: " + logErr.Error()
		} else {
			diag.DockerLogsTail = tail
		}

		outDir := filepath.Join(dir, containerID)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			slog.Warn("container backend: diagnostics collector: create runtime dir failed", "container_id", containerID, "dir", outDir, "error", err)
			return
		}
		data, err := json.MarshalIndent(diag, "", "  ")
		if err != nil {
			slog.Warn("container backend: diagnostics collector: marshal failed", "container_id", containerID, "error", err)
			return
		}
		path := filepath.Join(outDir, diagnosticsFileName)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			slog.Warn("container backend: diagnostics collector: write failed", "container_id", containerID, "path", path, "error", err)
		}
	}
}

// captureContainerLogsTail fetches up to diagnosticsMaxLogBytes of
// containerID's docker-side log buffer (both stdout and stderr, combined,
// matching this backend's own single-combined-stream transcript contract).
// The result is not demuxed — this is a troubleshooting artifact, not a
// byte-exact transcript, so leaving the 8-byte framing in place is fine.
func captureContainerLogsTail(ctx context.Context, api dockerAPI, containerID string) (string, error) {
	logs, err := api.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "200",
	})
	if err != nil {
		return "", err
	}
	defer logs.Close()

	data, err := io.ReadAll(io.LimitReader(logs, diagnosticsMaxLogBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

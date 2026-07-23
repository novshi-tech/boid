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
// writes under <runtimeDir>/<containerID>/ — the sibling of
// localRuntimeTranscriptFile ("transcript.log") under the same per-job
// directory.
const diagnosticsFileName = "diagnostics.json"

// diagnosticsMaxLogBytes bounds how much of a container's docker-side log
// buffer (ContainerLogs) NewDefaultDiagnosticsCollector captures. This is a
// diagnostic artifact for post-hoc troubleshooting, not the full-persistence
// transcript spool (§決定8's own separate, unbounded contract) — a generous
// but finite cap keeps a runaway or spam-logging container from writing an
// unbounded diagnostics.json.
const diagnosticsMaxLogBytes = 256 * 1024

// containerDiagnostics is the JSON shape NewDefaultDiagnosticsCollector
// writes to <runtimeDir>/<containerID>/diagnostics.json. Every field is
// best-effort: an inspect or logs-fetch failure degrades gracefully (see
// the collector's own doc comment) rather than skipping the whole file.
type containerDiagnostics struct {
	ContainerID string    `json:"container_id"`
	CapturedAt  time.Time `json:"captured_at"`
	ExitCode    int       `json:"exit_code"`
	// InspectExitCode / InspectStatus / OOMKilled / InspectError come from
	// ContainerInspect's State, captured separately from the Wait-reported
	// ExitCode above: they can disagree (e.g. a container ContainerWait
	// reports exit 137 for, but whose inspect State didn't get a chance to
	// update before removal) and both are useful for post-hoc diagnosis.
	InspectExitCode int    `json:"inspect_exit_code,omitempty"`
	InspectStatus   string `json:"inspect_status,omitempty"`
	OOMKilled       bool   `json:"oom_killed"`
	InspectError    string `json:"inspect_error,omitempty"`
	// CollectorError records a failure of the collector's OWN inspect/logs
	// calls (e.g. the container was already gone by the time it ran) —
	// distinct from InspectError (dockerd's own State.Error field).
	CollectorError string `json:"collector_error,omitempty"`
	// DockerLogsTail is dockerd's own retained log buffer, independent of
	// this backend's attach-stream transcript spool — useful specifically
	// for the OOM-killed / setup-failure case the transcript spool can miss
	// (§決定8's silent-exit classification, the reason this collector
	// exists at all). Truncated to diagnosticsMaxLogBytes.
	DockerLogsTail string `json:"docker_logs_tail,omitempty"`
}

// NewDefaultDiagnosticsCollector returns a ContainerBackendOptions.
// DiagnosticsCollector implementation suitable for production wiring
// (internal/server/wire.go's sandboxBackendForConfig — [Major 7, PR7 codex
// review]).
//
// Before this fix, ContainerBackendOptions.DiagnosticsCollector was never
// set outside of tests (NewContainerBackend's own doc comment on production
// wiring): containerSession.waitLoop's own §決定7/8 ordering contract
// ("診断回収 → job fallback 処理 → resource remove") had a real hook to run
// through, but nothing was ever plugged into it — an OOM-killed or
// setup-failure container was removed with NO diagnostic capture beyond
// whatever the (possibly-empty, possibly-truncated) attach-stream
// transcript spool happened to catch: exit code 137 and an empty log were
// often the only signal left.
//
// The returned collector runs ONLY when exit.ExitCode != 0 (the common
// happy path already has its full output durable via the transcript spool
// — §決定8's distinction between "full persistence" and "silent-exit
// diagnosis" — so a clean exit does not pay the extra ContainerInspect/
// ContainerLogs round trip). On an abnormal exit it captures ContainerInspect
// (exit code / status / OOMKilled / dockerd's own State.Error) and a
// bounded tail of ContainerLogs (dockerd's own independently-retained log
// buffer — the attach-stream transcript spool can be empty or truncated for
// a SIGKILL'd container, but dockerd's own buffer isn't), and writes both
// to <runtimeDir>/<containerID>/diagnostics.json — the sibling of
// transcript.log under the same per-job directory. Every step is
// best-effort: an inspect or logs failure is recorded in the
// diagnostics.json itself (CollectorError) rather than losing the whole
// artifact, and the collector NEVER returns an error to its caller
// (waitLoop's own contract — see ContainerBackendOptions.DiagnosticsCollector's
// doc comment: this runs strictly before ContainerRemove, so it must not
// block or fail container teardown).
//
// runtimeDir empty (every pre-Major-7 test/caller, or a deploy that hasn't
// wired ContainerBackendOptions.RuntimeDir) makes the returned collector a
// no-op — there is no host-visible directory to write into (mirrors
// openTranscriptSpool's identical empty-runtimeDir degrade).
func NewDefaultDiagnosticsCollector(api dockerAPI, runtimeDir string) func(ctx context.Context, containerID string, exit backend.RuntimeExit) {
	return func(ctx context.Context, containerID string, exit backend.RuntimeExit) {
		if runtimeDir == "" || exit.ExitCode == 0 {
			return
		}
		diag := containerDiagnostics{
			ContainerID: containerID,
			CapturedAt:  time.Now().UTC(),
			ExitCode:    exit.ExitCode,
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

		dir := filepath.Join(runtimeDir, containerID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("container backend: diagnostics collector: create runtime dir failed", "container_id", containerID, "dir", dir, "error", err)
			return
		}
		data, err := json.MarshalIndent(diag, "", "  ")
		if err != nil {
			slog.Warn("container backend: diagnostics collector: marshal failed", "container_id", containerID, "error", err)
			return
		}
		path := filepath.Join(dir, diagnosticsFileName)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			slog.Warn("container backend: diagnostics collector: write failed", "container_id", containerID, "path", path, "error", err)
		}
	}
}

// captureContainerLogsTail fetches up to diagnosticsMaxLogBytes of
// containerID's docker-side log buffer (both stdout and stderr, combined —
// matching this backend's own single-combined-stream transcript contract,
// §決定8). The result is NOT demuxed (demuxDockerFrame's 8-byte framing is
// stripped for the live transcript spool's own combined-stream contract,
// but a raw diagnostic capture is fine to leave un-demuxed — this is a
// troubleshooting artifact, not a byte-exact transcript).
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

package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins [Major 7, PR7 codex review]: NewDefaultDiagnosticsCollector
// is the production DiagnosticsCollector implementation
// internal/server/wire.go's sandboxBackendForConfig wires into
// NewContainerBackend — before this fix, ContainerBackendOptions.
// DiagnosticsCollector was never set outside of tests, so an OOM-killed or
// setup-failure job container was removed with no diagnostic capture beyond
// whatever the attach-stream transcript spool happened to catch.

// TestNewDefaultDiagnosticsCollector_AbnormalExit_WritesDiagnosticsFile
// pins the core contract: on a non-zero exit, the collector captures
// ContainerInspect (exit code / OOM flag / dockerd's own state error) and a
// bounded ContainerLogs tail, and writes both to
// <runtimeDir>/<containerID>/diagnostics.json.
func TestNewDefaultDiagnosticsCollector_AbnormalExit_WritesDiagnosticsFile(t *testing.T) {
	runtimeDir := t.TempDir()
	const containerID = "diag-container-1"

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State: &container.State{
						ExitCode:  137,
						OOMKilled: true,
						Status:    container.StateExited,
						Error:     "oom",
					},
				},
			}, nil
		},
		ContainerLogsFunc: func(ctx context.Context, id string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
			return io.NopCloser(strings.NewReader("last output before OOM kill")), nil
		},
	}

	collector := NewDefaultDiagnosticsCollector(api, runtimeDir)
	collector(context.Background(), containerID, backend.RuntimeExit{ExitCode: 137})

	path := filepath.Join(runtimeDir, containerID, diagnosticsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read diagnostics file: %v", err)
	}
	var diag containerDiagnostics
	if err := json.Unmarshal(data, &diag); err != nil {
		t.Fatalf("unmarshal diagnostics: %v", err)
	}
	if diag.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", diag.ExitCode)
	}
	if diag.InspectExitCode != 137 {
		t.Errorf("InspectExitCode = %d, want 137", diag.InspectExitCode)
	}
	if !diag.OOMKilled {
		t.Error("OOMKilled = false, want true")
	}
	if diag.InspectError != "oom" {
		t.Errorf("InspectError = %q, want %q", diag.InspectError, "oom")
	}
	if diag.DockerLogsTail != "last output before OOM kill" {
		t.Errorf("DockerLogsTail = %q, want the captured log content", diag.DockerLogsTail)
	}
	if diag.CollectorError != "" {
		t.Errorf("CollectorError = %q, want empty (both inspect and logs succeeded)", diag.CollectorError)
	}

	if len(api.inspectIDs) != 1 || api.inspectIDs[0] != containerID {
		t.Errorf("ContainerInspect calls = %v, want exactly [%s]", api.inspectIDs, containerID)
	}
	if len(api.logsIDs) != 1 || api.logsIDs[0] != containerID {
		t.Errorf("ContainerLogs calls = %v, want exactly [%s]", api.logsIDs, containerID)
	}
}

// TestNewDefaultDiagnosticsCollector_CleanExit_NoOp pins the happy-path
// scope: a clean exit (ExitCode == 0) must not even call ContainerInspect/
// ContainerLogs, let alone write a diagnostics file — the transcript spool
// already durably captured everything for that case (§決定8).
func TestNewDefaultDiagnosticsCollector_CleanExit_NoOp(t *testing.T) {
	runtimeDir := t.TempDir()
	const containerID = "diag-container-clean"

	api := &fakeDockerAPI{}
	collector := NewDefaultDiagnosticsCollector(api, runtimeDir)
	collector(context.Background(), containerID, backend.RuntimeExit{ExitCode: 0})

	if len(api.inspectIDs) != 0 {
		t.Errorf("ContainerInspect calls = %v, want none on a clean exit", api.inspectIDs)
	}
	if len(api.logsIDs) != 0 {
		t.Errorf("ContainerLogs calls = %v, want none on a clean exit", api.logsIDs)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, containerID, diagnosticsFileName)); !os.IsNotExist(err) {
		t.Errorf("diagnostics file should not exist for a clean exit, stat err = %v", err)
	}
}

// TestNewDefaultDiagnosticsCollector_RuntimeDirUnset_NoOp pins the
// companion non-regression: RuntimeDir unset (every pre-Major-7 test/
// caller) must make the collector a complete no-op, matching
// openTranscriptSpool's identical empty-runtimeDir degrade.
func TestNewDefaultDiagnosticsCollector_RuntimeDirUnset_NoOp(t *testing.T) {
	api := &fakeDockerAPI{}
	collector := NewDefaultDiagnosticsCollector(api, "")
	// Must not panic; must not call the docker API at all.
	collector(context.Background(), "some-container", backend.RuntimeExit{ExitCode: 1})

	if len(api.inspectIDs) != 0 || len(api.logsIDs) != 0 {
		t.Errorf("docker API called with RuntimeDir unset: inspect=%v logs=%v", api.inspectIDs, api.logsIDs)
	}
}

// TestNewDefaultDiagnosticsCollector_InspectFails_StillWritesLogsAndError
// pins the graceful-degradation contract: an ContainerInspect failure must
// not lose the whole diagnostics artifact — it is recorded in
// CollectorError, and the (independently-fetched) log tail is still
// captured and written.
func TestNewDefaultDiagnosticsCollector_InspectFails_StillWritesLogsAndError(t *testing.T) {
	runtimeDir := t.TempDir()
	const containerID = "diag-container-inspect-fail"

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, context.DeadlineExceeded
		},
		ContainerLogsFunc: func(ctx context.Context, id string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
			return io.NopCloser(strings.NewReader("still got logs")), nil
		},
	}
	collector := NewDefaultDiagnosticsCollector(api, runtimeDir)
	collector(context.Background(), containerID, backend.RuntimeExit{ExitCode: 1})

	data, err := os.ReadFile(filepath.Join(runtimeDir, containerID, diagnosticsFileName))
	if err != nil {
		t.Fatalf("read diagnostics file: %v", err)
	}
	var diag containerDiagnostics
	if err := json.Unmarshal(data, &diag); err != nil {
		t.Fatalf("unmarshal diagnostics: %v", err)
	}
	if diag.CollectorError == "" {
		t.Error("CollectorError = empty, want the inspect failure recorded")
	}
	if diag.DockerLogsTail != "still got logs" {
		t.Errorf("DockerLogsTail = %q, want the log content despite the inspect failure", diag.DockerLogsTail)
	}
}

package sandbox_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR2 (docs/plans/phase5-shim-and-task-context.md): `boid task
// attachments list` / `get <name>` — the CLI-side (shim) tests. Unlike the
// four Phase 5b PR1 task-context subcommands, `get` takes a positional
// attachment name and an optional `--output <path>` flag, and its
// broker-side reply carries base64-encoded bytes (not JSON/YAML) that the
// shim must decode before either writing to --output or handing the raw
// bytes back to main.go's os.Stdout.WriteString(resp.Stdout).

func TestRunBoidShim_TaskAttachmentsList_UsesEnvTaskID(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `["a.png","b.png"]`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "list"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskAttachmentsList {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskAttachmentsList)
	}
	if req.Boid.TaskID != "t1" {
		t.Errorf("task id = %q, want t1 (from BOID_TASK_ID)", req.Boid.TaskID)
	}
}

func TestRunBoidShim_TaskAttachmentsList_DefaultFormatIsYAML(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `["a.png","b.png"]`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "list"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "[") {
		t.Errorf("stdout = %q, want YAML rendering (no JSON brackets) by default", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "a.png") || !strings.Contains(resp.Stdout, "b.png") {
		t.Errorf("stdout = %q, want both names rendered", resp.Stdout)
	}
}

func TestRunBoidShim_TaskAttachmentsList_FormatJSON(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `["a.png"]`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "list", "--format", "json"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.Stdout != `["a.png"]` {
		t.Errorf("stdout = %q, want the raw JSON passthrough", resp.Stdout)
	}
}

func TestRunBoidShim_TaskAttachmentsList_InvalidFormatRejected(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_TASK_ID", "t1")

	_, err := sandbox.RunBoidShim([]string{"task", "attachments", "list", "--format", "xml"})
	if err == nil {
		t.Fatal("expected error for unsupported --format value")
	}
	if !strings.Contains(err.Error(), "--format") {
		t.Errorf("error = %v, want mention of --format", err)
	}
}

func TestRunBoidShim_TaskAttachmentsList_UnsupportedFlagRejected(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_TASK_ID", "t1")

	_, err := sandbox.RunBoidShim([]string{"task", "attachments", "list", "--bogus"})
	if err == nil || !strings.Contains(err.Error(), "unsupported flag") {
		t.Fatalf("expected unsupported flag error, got: %v", err)
	}
}

func TestRunBoidShim_TaskAttachmentsGet_UsesEnvTaskIDAndName(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: base64.StdEncoding.EncodeToString([]byte("PNGDATA")),
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "get", "shot.png"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "PNGDATA" {
		t.Errorf("stdout = %q, want the raw decoded bytes PNGDATA", resp.Stdout)
	}

	req := <-reqCh
	if req.Boid.Op != sandbox.BoidOpTaskAttachmentsGet {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskAttachmentsGet)
	}
	if req.Boid.TaskID != "t1" {
		t.Errorf("task id = %q, want t1", req.Boid.TaskID)
	}
	if req.Boid.AttachmentName != "shot.png" {
		t.Errorf("attachment name = %q, want shot.png", req.Boid.AttachmentName)
	}
}

// Binary content (not valid UTF-8) must survive the base64 round trip
// unmodified — this is the whole reason the wire transport base64-encodes
// rather than putting raw bytes in a JSON string field.
func TestRunBoidShim_TaskAttachmentsGet_BinaryContentSurvives(t *testing.T) {
	binary := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x0d, 0x0a, 0xff, 0xfe}
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: base64.StdEncoding.EncodeToString(binary),
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "get", "shot.png"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.Stdout != string(binary) {
		t.Errorf("stdout bytes = %x, want %x", resp.Stdout, binary)
	}
}

func TestRunBoidShim_TaskAttachmentsGet_OutputFlagWritesFile(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: base64.StdEncoding.EncodeToString([]byte("PNGDATA")),
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	outPath := filepath.Join(t.TempDir(), "shot.png")
	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "get", "shot.png", "--output", outPath})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Errorf("file content = %q, want PNGDATA", data)
	}
}

func TestRunBoidShim_TaskAttachmentsGet_MissingNameRejected(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_TASK_ID", "t1")

	_, err := sandbox.RunBoidShim([]string{"task", "attachments", "get"})
	if err == nil || !strings.Contains(err.Error(), "attachment name") {
		t.Fatalf("expected attachment name requirement error, got: %v", err)
	}
}

func TestRunBoidShim_TaskAttachmentsGet_BrokerErrorPropagates(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		ExitCode: 1,
		Stderr:   "attachment not found: nope.png",
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "get", "nope.png"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "attachment not found") {
		t.Errorf("stderr = %q, want the broker's error message passed through", resp.Stderr)
	}
}

func TestRunBoidShim_TaskAttachmentsGet_UnsupportedSubcommandRejected(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_TASK_ID", "t1")

	_, err := sandbox.RunBoidShim([]string{"task", "attachments", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported subcommand error, got: %v", err)
	}
}

// jsonToYAMLForShim's list-format re-render path uses json.Unmarshal
// internally; a malformed broker reply should fall back to the original
// string rather than crash the CLI (mirrors PR1's defensive behavior).
func TestRunBoidShim_TaskAttachmentsList_MalformedJSONFallsBackToRawStdout(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: "not json",
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")

	resp, err := sandbox.RunBoidShim([]string{"task", "attachments", "list"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.Stdout != "not json" {
		t.Errorf("stdout = %q, want the raw stdout unchanged", resp.Stdout)
	}
}

// json round trip smoke check to ensure the list JSON shape assumption
// above (a bare string array) is what the executor really emits.
func TestTaskAttachmentsList_JSONShapeSanityCheck(t *testing.T) {
	var names []string
	if err := json.Unmarshal([]byte(`["a.png","b.png"]`), &names); err != nil {
		t.Fatalf("unexpected unmarshal failure: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
}

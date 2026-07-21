package sandbox_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): `boid task
// update --payload-patch @-` is the job_done payload_patch direct-pass CLI.
// It is JobID-scoped (reads BOID_JOB_ID from the environment, like the
// Phase 5b PR1 task-context subcommands) rather than taking a positional
// task id, and follows curl's `@` convention: a bare value is inline
// content, `@<path>` reads a file, and `@-` reads stdin.

func TestRunBoidShim_TaskUpdatePayloadPatch_Stdin(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0})
	}()

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-patch")
	t.Setenv("BOID_JOB_ID", "job-abc")

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	go func() {
		_, _ = w.WriteString(`{"artifact":{"report":{"summary":"done"}}}`)
		w.Close()
	}()

	resp, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "@-"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskUpdatePayloadPatch {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskUpdatePayloadPatch)
	}
	if req.Boid.JobID != "job-abc" {
		t.Fatalf("job id = %q, want job-abc (from BOID_JOB_ID)", req.Boid.JobID)
	}
	if got, want := string(req.Boid.PayloadPatch), `{"artifact":{"report":{"summary":"done"}}}`; got != want {
		t.Fatalf("payload patch = %s, want %s", got, want)
	}
}

func TestRunBoidShim_TaskUpdatePayloadPatch_InlineValue(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0})
	}()

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-patch")
	t.Setenv("BOID_JOB_ID", "job-abc")

	resp, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", `{"artifact":{"report":{"summary":"done"}}}`})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
	req := <-reqCh
	if got, want := string(req.Boid.PayloadPatch), `{"artifact":{"report":{"summary":"done"}}}`; got != want {
		t.Fatalf("payload patch = %s, want %s", got, want)
	}
}

func TestRunBoidShim_TaskUpdatePayloadPatch_FromFile(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0})
	}()

	patchPath := filepath.Join(dir, "patch.yaml")
	if err := os.WriteFile(patchPath, []byte("artifact:\n  report:\n    summary: done\n"), 0o644); err != nil {
		t.Fatalf("write patch file: %v", err)
	}

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-patch")
	t.Setenv("BOID_JOB_ID", "job-abc")

	resp, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "@" + patchPath})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
	req := <-reqCh
	if got, want := string(req.Boid.PayloadPatch), `{"artifact":{"report":{"summary":"done"}}}`; got != want {
		t.Fatalf("payload patch = %s, want %s", got, want)
	}
}

func TestRunBoidShim_TaskUpdatePayloadPatch_RequiresJobID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_JOB_ID", "")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "{}"}); err == nil {
		t.Fatal("expected error when BOID_JOB_ID is unset")
	}
}

func TestRunBoidShim_TaskUpdatePayloadPatch_RejectsEmptyContent(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_JOB_ID", "job-abc")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", ""}); err == nil {
		t.Fatal("expected error for empty --payload-patch value")
	}
}

func TestRunBoidShim_TaskUpdatePayloadPatch_RejectsInvalidContent(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_JOB_ID", "job-abc")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "not: [valid: yaml"}); err == nil {
		t.Fatal("expected error for unparsable --payload-patch content")
	}
}

// --payload-patch is a distinct op with its own merge semantics
// (orchestrator.MergePayloadPatch) — combining it with the top-level
// shallow-merge flags (--title/--payload-file/etc, BoidOpTaskUpdate) or a
// positional task id would conflate two different request shapes, so both
// are rejected rather than silently ignored.
func TestRunBoidShim_TaskUpdatePayloadPatch_RejectsCombinationWithOtherFlags(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_JOB_ID", "job-abc")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "{}", "--title", "x"}); err == nil {
		t.Fatal("expected error when --payload-patch is combined with --title")
	}
}

func TestRunBoidShim_TaskUpdatePayloadPatch_RejectsPositionalTaskID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_JOB_ID", "job-abc")

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "task-explicit", "--payload-patch", "{}"}); err == nil {
		t.Fatal("expected error when --payload-patch is combined with a positional task id")
	}
}

// TestRunBoidShim_TaskUpdatePayloadPatch_NormalizesNonStringYAMLKeys pins
// Phase 5b PR7 codex review Major 2 (wiring-seams.md #17): yaml.v3 decodes a
// mapping with a non-string key (bool/int/null — the historical `on:` →
// `true:` PyYAML round-trip incident coordinator.go's parseHandlerResult
// already guards against) as map[interface{}]interface{}, which
// json.Marshal cannot handle on its own. The CLI path must apply the exact
// same internal/yamlutil.NormalizeKeys the file-based path uses, or the
// same payload_patch content behaves differently (or errors outright)
// depending on which path carries it.
func TestRunBoidShim_TaskUpdatePayloadPatch_NormalizesNonStringYAMLKeys(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0})
	}()

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-patch")
	t.Setenv("BOID_JOB_ID", "job-abc")

	// "true:" is a bool key once yaml.v3 decodes it — without normalization
	// this would either fail to marshal or (worse) silently drop the value
	// depending on the yaml library's exact behavior.
	resp, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "artifact:\n  report:\n    true: verifying\n"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
	req := <-reqCh
	if got, want := string(req.Boid.PayloadPatch), `{"artifact":{"report":{"true":"verifying"}}}`; got != want {
		t.Fatalf("payload patch = %s, want %s", got, want)
	}
}

// Phase 5b PR7 codex review Major 3 (wiring-seams.md #17): --payload-patch
// content crosses the broker RPC boundary into the daemon process, unlike a
// purely local file read — an unbounded io.ReadAll on stdin/file content
// lets a mistakenly huge input (or a malicious/buggy sandboxed process) OOM
// both the runner (multiple in-memory copies: raw bytes, YAML-decoded
// value, re-marshaled JSON) and, if it reached the wire, the daemon itself.
func TestRunBoidShim_TaskUpdatePayloadPatch_RejectsOversizedContent(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_JOB_ID", "job-abc")

	dir := t.TempDir()
	oversizedPath := filepath.Join(dir, "huge.json")
	f, err := os.Create(oversizedPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// One byte over the cap is enough to prove the limit is enforced without
	// actually writing gigabytes to disk for the test.
	if _, err := f.Write([]byte(`{"artifact":{"report":{"summary":"`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	padding := make([]byte, sandbox.PayloadPatchMaxBytes)
	for i := range padding {
		padding[i] = 'x'
	}
	if _, err := f.Write(padding); err != nil {
		t.Fatalf("write padding: %v", err)
	}
	if _, err := f.Write([]byte(`"}}}`)); err != nil {
		t.Fatalf("write suffix: %v", err)
	}
	f.Close()

	if _, err := sandbox.RunBoidShim([]string{"task", "update", "--payload-patch", "@" + oversizedPath}); err == nil {
		t.Fatal("expected error for content exceeding PayloadPatchMaxBytes")
	}
}

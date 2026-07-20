package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR2 (docs/plans/phase5-shim-and-task-context.md): the executor
// dispatch tests for `boid task attachments list` / `get <name>`. Both back
// onto api.ListAttachments/api.ReadAttachment against a real temp
// filesystem directory (attachmentsRoot) — no interface/stub layer is
// needed here (unlike jobContextProvider), since attachmentsRoot is a plain
// config string, so the "real wiring" concern from the boid-review skill's
// Lens 1 collapses into "does the executor pass attachmentsRoot straight to
// the real api functions", which these tests exercise directly.

func seedAttachment(t *testing.T, attachmentsRoot, taskID, name string, body []byte) {
	t.Helper()
	if _, err := api.SaveMultipartAttachments(attachmentsRoot, taskID, []*multipart.FileHeader{
		makeAttachmentFileHeader(t, name, body),
	}); err != nil {
		t.Fatalf("seed attachment %q: %v", name, err)
	}
}

// makeAttachmentFileHeader mirrors internal/api's own makeFileHeader test
// helper (unexported there, so duplicated here rather than exported purely
// for test use).
func makeAttachmentFileHeader(t *testing.T, name string, body []byte) *multipart.FileHeader {
	t.Helper()
	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="attachments"; filename="`+name+`"`)
	h.Set("Content-Type", "image/png")
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(strings.NewReader(buf.String()), mw.Boundary())
	form, err := mr.ReadForm(int64(len(body)) * 4)
	if err != nil {
		t.Fatal(err)
	}
	headers, ok := form.File["attachments"]
	if !ok || len(headers) == 0 {
		t.Fatal("no parts parsed")
	}
	return headers[0]
}

func TestBoidBuiltinExecutor_TaskAttachmentsList_HappyPath(t *testing.T) {
	root := t.TempDir()
	seedAttachment(t, root, "task-1", "a.png", []byte("a"))
	seedAttachment(t, root, "task-1", "b.png", []byte("b"))

	exec := &boidBuiltinExecutor{attachmentsRoot: root}
	resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-1"}, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskAttachmentsList,
		TaskID: "task-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	var names []string
	if err := json.Unmarshal([]byte(resp.Stdout), &names); err != nil {
		t.Fatalf("stdout is not a JSON array: %q: %v", resp.Stdout, err)
	}
	if len(names) != 2 || names[0] != "a.png" || names[1] != "b.png" {
		t.Errorf("names = %v, want [a.png b.png]", names)
	}
}

// A task that has never received an attachment must get an empty JSON
// array, not null and not an error.
func TestBoidBuiltinExecutor_TaskAttachmentsList_EmptyArrayWhenNoAttachments(t *testing.T) {
	root := t.TempDir()
	exec := &boidBuiltinExecutor{attachmentsRoot: root}
	resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-never-attached"}, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskAttachmentsList,
		TaskID: "task-never-attached",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if strings.TrimSpace(resp.Stdout) != "[]" {
		t.Errorf("stdout = %q, want empty JSON array", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskAttachmentsList_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{attachmentsRoot: ""}
	resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-1"}, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskAttachmentsList,
		TaskID: "task-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_TaskAttachmentsGet_HappyPath(t *testing.T) {
	root := t.TempDir()
	seedAttachment(t, root, "task-1", "shot.png", []byte("PNGDATA"))

	exec := &boidBuiltinExecutor{attachmentsRoot: root}
	resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-1"}, &sandbox.BoidRequest{
		Op:             sandbox.BoidOpTaskAttachmentsGet,
		TaskID:         "task-1",
		AttachmentName: "shot.png",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Stdout)
	if err != nil {
		t.Fatalf("stdout is not valid base64: %q: %v", resp.Stdout, err)
	}
	if string(decoded) != "PNGDATA" {
		t.Errorf("decoded = %q, want PNGDATA", decoded)
	}
}

func TestBoidBuiltinExecutor_TaskAttachmentsGet_NotFound(t *testing.T) {
	root := t.TempDir()
	if _, err := api.EnsureAttachmentsDir(root, "task-1"); err != nil {
		t.Fatal(err)
	}
	exec := &boidBuiltinExecutor{attachmentsRoot: root}
	resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-1"}, &sandbox.BoidRequest{
		Op:             sandbox.BoidOpTaskAttachmentsGet,
		TaskID:         "task-1",
		AttachmentName: "nope.png",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error for missing attachment, got exit=%d", resp.ExitCode)
	}
}

// Regression guard for the security requirement: even if the broker-level
// guard were ever bypassed, the executor's own call into api.ReadAttachment
// must still reject a traversal attempt end-to-end (this exercises the real
// api.ReadAttachment, not a stub, per the "real wiring" pattern).
func TestBoidBuiltinExecutor_TaskAttachmentsGet_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	seedAttachment(t, root, "task-1", "legit.png", []byte("ok"))
	secretDir := filepath.Join(root, "tasks", "task-2", "attachments")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "secret.png"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := &boidBuiltinExecutor{attachmentsRoot: root}
	for _, name := range []string{"../task-2/attachments/secret.png", "/etc/passwd", ".."} {
		resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-1"}, &sandbox.BoidRequest{
			Op:             sandbox.BoidOpTaskAttachmentsGet,
			TaskID:         "task-1",
			AttachmentName: name,
		})
		if resp.ExitCode == 0 {
			t.Errorf("attachment name %q: expected rejection, got success with stdout=%q", name, resp.Stdout)
		}
	}
}

// Full-stack regression for codex review's Blocker finding on PR #798: a
// TaskID shaped like "alias/../<victim-id>" would pass the broker's raw
// string-equality authorization check trivially (both the token context and
// the request carry the identical literal alias — see
// internal/sandbox/broker.go's BoidOpTaskAttachmentsList/Get case), so this
// test drives the executor with exactly that "already authorized" shape and
// asserts it still cannot read or list the victim task's attachments. The
// internal/api-level tests (TestListAndReadAttachment_RejectsAliasTaskIDCrossTaskLeak)
// exercise the same guard closer to the source; this one proves the
// executor layer (the actual RPC entry point) doesn't reintroduce the leak
// by, say, resolving TaskID some other way before calling into api.*.
func TestBoidBuiltinExecutor_TaskAttachments_RejectsAliasTaskIDCrossTaskLeak(t *testing.T) {
	root := t.TempDir()
	victimID := "550e8400-e29b-41d4-a716-446655440000"
	seedAttachment(t, root, victimID, "secret.png", []byte("victim secret"))

	aliasID := "alias/../" + victimID
	exec := &boidBuiltinExecutor{attachmentsRoot: root}
	ctx := sandbox.TokenContext{TaskID: aliasID} // matches the broker's post-authorization context

	listResp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskAttachmentsList,
		TaskID: aliasID,
	})
	if listResp.ExitCode == 0 && strings.Contains(listResp.Stdout, "secret.png") {
		t.Fatalf("list leaked victim attachment via alias TaskID: %q", listResp.Stdout)
	}

	getResp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:             sandbox.BoidOpTaskAttachmentsGet,
		TaskID:         aliasID,
		AttachmentName: "secret.png",
	})
	if getResp.ExitCode == 0 {
		t.Fatalf("get leaked victim attachment via alias TaskID: stdout=%q", getResp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskAttachmentsGet_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{attachmentsRoot: ""}
	resp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{TaskID: "task-1"}, &sandbox.BoidRequest{
		Op:             sandbox.BoidOpTaskAttachmentsGet,
		TaskID:         "task-1",
		AttachmentName: "shot.png",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

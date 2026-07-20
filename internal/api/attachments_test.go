package api

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeAttachmentName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"plain png", "screenshot.png", "screenshot.png", false},
		{"with dash and digits", "image-1.png", "image-1.png", false},
		{"underscore + log", "trace_01.log", "trace_01.log", false},
		// filepath.Base strips directory components, so "../../etc/passwd"
		// reduces to "passwd" — which then fails the extension allowlist
		// (no extension), not the directory check itself. Either way:
		// rejected at the boundary.
		{"path-traversal stripped to no-ext", "../../etc/passwd", "", true},
		{"slash stripped to legal name", "subdir/file.png", "file.png", false},
		{"dotfile rejected", ".env", "", true},
		{"empty rejected", "", "", true},
		{"dot rejected", ".", "", true},
		{"dotdot rejected", "..", "", true},
		{"space rejected", "my file.png", "", true},
		{"unicode rejected", "日本語.png", "", true},
		{"unknown extension", "binary.exe", "", true},
		{"too long", strings.Repeat("a", 256) + ".png", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SanitizeAttachmentName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("SanitizeAttachmentName(%q) err=%v wantErr=%v", tc.input, err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("SanitizeAttachmentName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestResolveUniqueName(t *testing.T) {
	dir := t.TempDir()
	for _, existing := range []string{"a.png", "a-1.png"} {
		if err := os.WriteFile(filepath.Join(dir, existing), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveUniqueName(dir, "a.png")
	if err != nil {
		t.Fatalf("resolveUniqueName: %v", err)
	}
	if got != "a-2.png" {
		t.Errorf("got %q, want a-2.png (a.png and a-1.png already exist)", got)
	}
}

func TestEnsureAttachmentsDir(t *testing.T) {
	dataHome := t.TempDir()
	dir, err := EnsureAttachmentsDir(dataHome, "task-1")
	if err != nil {
		t.Fatalf("EnsureAttachmentsDir: %v", err)
	}
	want := filepath.Join(dataHome, "tasks", "task-1", "attachments")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("attachments dir not created: stat=%v err=%v", info, err)
	}
	if _, err := EnsureAttachmentsDir("", "task-1"); err == nil {
		t.Errorf("EnsureAttachmentsDir(\"\", ...) should fail")
	}
}

func TestSaveMultipartAttachments_HappyPath(t *testing.T) {
	dataHome := t.TempDir()
	files := []*multipart.FileHeader{
		makeFileHeader(t, "shot.png", "image/png", []byte("PNGDATA")),
		makeFileHeader(t, "trace.log", "text/plain", []byte("line1\nline2\n")),
	}
	saved, err := SaveMultipartAttachments(dataHome, "task-x", files)
	if err != nil {
		t.Fatalf("SaveMultipartAttachments: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}
	for _, name := range saved {
		body, err := os.ReadFile(filepath.Join(dataHome, "tasks", "task-x", "attachments", name))
		if err != nil {
			t.Errorf("read saved %q: %v", name, err)
		}
		if len(body) == 0 {
			t.Errorf("saved %q is empty", name)
		}
	}
}

func TestSaveMultipartAttachments_RejectsOversize(t *testing.T) {
	dataHome := t.TempDir()
	big := bytes.Repeat([]byte{0xff}, int(AttachmentMaxFileBytes+1))
	files := []*multipart.FileHeader{makeFileHeader(t, "big.png", "image/png", big)}
	if _, err := SaveMultipartAttachments(dataHome, "task-y", files); err == nil {
		t.Errorf("expected per-file size cap rejection")
	}
}

func TestSaveMultipartAttachments_NameCollision(t *testing.T) {
	dataHome := t.TempDir()
	files := []*multipart.FileHeader{
		makeFileHeader(t, "dup.png", "image/png", []byte("a")),
		makeFileHeader(t, "dup.png", "image/png", []byte("b")),
	}
	saved, err := SaveMultipartAttachments(dataHome, "task-z", files)
	if err != nil {
		t.Fatalf("SaveMultipartAttachments: %v", err)
	}
	if len(saved) != 2 || saved[0] != "dup.png" || saved[1] != "dup-1.png" {
		t.Errorf("saved = %v, want [dup.png dup-1.png]", saved)
	}
}

func TestSaveMultipartAttachments_RejectsBadName(t *testing.T) {
	dataHome := t.TempDir()
	// Spaces are not in the ^[A-Za-z0-9._-]+$ allowlist, and filepath.Base
	// won't strip them (unlike "../foo" which becomes a legal "foo" basename).
	// This is the actual class of name we want SaveMultipartAttachments to
	// refuse rather than silently coerce.
	files := []*multipart.FileHeader{
		makeFileHeader(t, "bad name.png", "image/png", []byte("x")),
	}
	_, err := SaveMultipartAttachments(dataHome, "task-bad", files)
	if err == nil {
		t.Errorf("expected sanitization error for 'bad name.png'")
	}
	// The aborted call should have left the dir empty (no half-write).
	entries, _ := os.ReadDir(filepath.Join(dataHome, "tasks", "task-bad", "attachments"))
	if len(entries) != 0 {
		t.Errorf("expected no leftover files, got %d", len(entries))
	}
}

func TestValidateAttachmentHeaders_TotalCap(t *testing.T) {
	// 4 files of 8 MB each = 32 MB total > 30 MB cap.
	const chunk = 8 * 1024 * 1024
	body := bytes.Repeat([]byte{0x00}, chunk)
	files := []*multipart.FileHeader{
		makeFileHeader(t, "a.png", "image/png", body),
		makeFileHeader(t, "b.png", "image/png", body),
		makeFileHeader(t, "c.png", "image/png", body),
		makeFileHeader(t, "d.png", "image/png", body),
	}
	if err := ValidateAttachmentHeaders(files); err == nil {
		t.Errorf("expected total cap to fail (4x8MB > 30MB)")
	}
}

// --- Phase 5b PR2 attachments RPCs (docs/plans/phase5-shim-and-task-context.md) ---
// ListAttachments / ReadAttachment are the filesystem-level functions the
// broker RPC (BoidOpTaskAttachmentsList/Get, internal/server/boid_executor.go)
// calls. They read from the exact same AttachmentsRootForTask directory the
// write path (SaveMultipartAttachments) populates and the dispatch-time RO
// bind (sandbox_builder.go) exposes, so drift between "what the bind shows"
// and "what the RPC returns" is structurally impossible — both derive from
// the same dataHome/taskID pair.

func TestListAttachments_HappyPath(t *testing.T) {
	dataHome := t.TempDir()
	if _, err := SaveMultipartAttachments(dataHome, "task-1", []*multipart.FileHeader{
		makeFileHeader(t, "b.png", "image/png", []byte("b")),
		makeFileHeader(t, "a.png", "image/png", []byte("a")),
	}); err != nil {
		t.Fatalf("seed attachments: %v", err)
	}

	names, err := ListAttachments(dataHome, "task-1")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(names) != 2 || names[0] != "a.png" || names[1] != "b.png" {
		t.Errorf("names = %v, want sorted [a.png b.png]", names)
	}
}

// A task that has never received an attachment (no directory at all) must
// return an empty, non-nil slice — not an error, and not nil (nil would
// marshal to JSON `null`, not `[]`, breaking the RPC's documented contract).
func TestListAttachments_NoDirReturnsEmptyNonNilSlice(t *testing.T) {
	dataHome := t.TempDir()
	names, err := ListAttachments(dataHome, "task-never-attached")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if names == nil {
		t.Fatal("names is nil, want an empty non-nil slice")
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want empty", names)
	}
}

// Subdirectories are never recursed into — attachments are a flat namespace
// (Phase 5b PR2 scope).
func TestListAttachments_SkipsSubdirectories(t *testing.T) {
	dataHome := t.TempDir()
	dir, err := EnsureAttachmentsDir(dataHome, "task-1")
	if err != nil {
		t.Fatalf("EnsureAttachmentsDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "hidden.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := ListAttachments(dataHome, "task-1")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(names) != 1 || names[0] != "top.png" {
		t.Errorf("names = %v, want only [top.png] (no recursion into nested/)", names)
	}
}

// A symlink inside the attachments directory whose target escapes the
// directory must not be advertised by list — defense-in-depth even though
// SaveMultipartAttachments (O_CREATE|O_EXCL on a plain file) never creates
// one itself.
func TestListAttachments_SkipsEscapingSymlink(t *testing.T) {
	dataHome := t.TempDir()
	dir, err := EnsureAttachmentsDir(dataHome, "task-1")
	if err != nil {
		t.Fatalf("EnsureAttachmentsDir: %v", err)
	}
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.png")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "escape.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legit.png"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := ListAttachments(dataHome, "task-1")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(names) != 1 || names[0] != "legit.png" {
		t.Errorf("names = %v, want only [legit.png] (escaping symlink filtered out)", names)
	}
}

func TestReadAttachment_HappyPath(t *testing.T) {
	dataHome := t.TempDir()
	if _, err := SaveMultipartAttachments(dataHome, "task-1", []*multipart.FileHeader{
		makeFileHeader(t, "shot.png", "image/png", []byte("PNGDATA")),
	}); err != nil {
		t.Fatalf("seed attachments: %v", err)
	}

	data, err := ReadAttachment(dataHome, "task-1", "shot.png")
	if err != nil {
		t.Fatalf("ReadAttachment: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Errorf("data = %q, want PNGDATA", data)
	}
}

func TestReadAttachment_NotFound(t *testing.T) {
	dataHome := t.TempDir()
	if _, err := EnsureAttachmentsDir(dataHome, "task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAttachment(dataHome, "task-1", "nope.png"); err == nil {
		t.Error("expected an error for a missing attachment")
	}
}

// The core security requirement for this PR: a name that tries to escape
// the attachments directory (relative traversal, absolute path, or a bare
// ".."/".") must be rejected before any path is ever constructed, rather
// than silently coerced (SanitizeAttachmentName's upload-time contract) or
// resolved against some other file.
func TestReadAttachment_PathTraversalRejected(t *testing.T) {
	dataHome := t.TempDir()
	dir, err := EnsureAttachmentsDir(dataHome, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	// A real file that a traversal attempt might try to reach if the guard
	// were broken (e.g. if it resolved ../<sibling-task>/attachments/secret.png).
	siblingDir := filepath.Join(dataHome, "tasks", "task-2", "attachments")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siblingDir, "secret.png"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legit.png"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"../task-2/attachments/secret.png",
		"../../tasks/task-2/attachments/secret.png",
		"../../etc/passwd",
		"/etc/passwd",
		filepath.Join(siblingDir, "secret.png"), // absolute path outright
		"..",
		".",
		"foo/../../task-2/attachments/secret.png",
		"a/b",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadAttachment(dataHome, "task-1", name); err == nil {
				t.Errorf("ReadAttachment(%q) succeeded, want a rejection", name)
			}
		})
	}
}

// A symlink placed inside the attachments directory (never created by the
// normal upload path, but not something ReadAttachment can assume away —
// see the RPC-vs-bind distinction in the security notes) must not let a
// plain, traversal-free basename reach a file outside the directory.
func TestReadAttachment_SymlinkEscapeRejected(t *testing.T) {
	dataHome := t.TempDir()
	dir, err := EnsureAttachmentsDir(dataHome, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.png")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "escape.png")); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadAttachment(dataHome, "task-1", "escape.png"); err == nil {
		t.Error("ReadAttachment via an escaping symlink succeeded, want a rejection")
	}
}

func TestReadAttachment_RejectsOversizedFile(t *testing.T) {
	dataHome := t.TempDir()
	dir, err := EnsureAttachmentsDir(dataHome, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	// Written directly (bypassing SaveMultipartAttachments' cap) to simulate
	// a file that ended up oversized some other way — ReadAttachment must
	// defend independently rather than trust the write path's own cap.
	big := bytes.Repeat([]byte{0xff}, int(AttachmentMaxFileBytes)+1)
	if err := os.WriteFile(filepath.Join(dir, "big.png"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAttachment(dataHome, "task-1", "big.png"); err == nil {
		t.Error("expected the size cap to reject an oversized attachment")
	}
}

// makeFileHeader builds a *multipart.FileHeader backed by an in-memory body.
// Used to drive SaveMultipartAttachments without spinning up an actual HTTP
// request; the only multipart.FileHeader fields the implementation cares
// about are Filename, Size and the body returned by Open().
func makeFileHeader(t *testing.T, name, contentType string, body []byte) *multipart.FileHeader {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="attachments"; filename="`+name+`"`)
	h.Set("Content-Type", contentType)
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
	// Re-parse to obtain a real *multipart.FileHeader with a working Open().
	mr := multipart.NewReader(buf, mw.Boundary())
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

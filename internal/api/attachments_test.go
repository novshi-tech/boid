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

package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Attachment-related constants. Values are also enforced client-side in
// web/static/boid-paste-attach.js; keep the two in sync.
const (
	AttachmentMaxFileBytes  int64 = 10 * 1024 * 1024 // 10 MB per file
	AttachmentMaxTotalBytes int64 = 30 * 1024 * 1024 // 30 MB per task dir
	AttachmentMaxNameBytes        = 255
)

// AttachmentAllowedExts is the file-extension allowlist for uploads.
// Both images (for screenshot paste) and a few text formats (for logs / json
// snippets) are accepted.
var AttachmentAllowedExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".txt":  true,
	".md":   true,
	".json": true,
	".log":  true,
}

// attachmentNameRe restricts filenames to a conservative ASCII subset so
// shell-quoting and path-traversal concerns disappear at the storage layer.
// Client-side filename generation in boid-paste-attach.js produces names that
// match this regex.
var attachmentNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// AttachmentsRootForTask resolves the on-disk attachments directory for the
// given task. dataHome is the data root (e.g. filepath.Dir(cfg.DBPath)), the
// same convention as runtimesDirFor.
func AttachmentsRootForTask(dataHome, taskID string) string {
	if dataHome == "" || taskID == "" {
		return ""
	}
	return filepath.Join(dataHome, "tasks", taskID, "attachments")
}

// EnsureAttachmentsDir creates the per-task attachments directory if it does
// not already exist. Used by handlers right after task creation / before
// accepting answer attachments so the bind mount has a non-empty source.
func EnsureAttachmentsDir(dataHome, taskID string) (string, error) {
	dir := AttachmentsRootForTask(dataHome, taskID)
	if dir == "" {
		return "", errors.New("empty attachments path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir attachments: %w", err)
	}
	return dir, nil
}

// SanitizeAttachmentName validates a user-supplied filename and returns the
// cleaned basename. It rejects anything that doesn't match attachmentNameRe,
// exceeds the byte cap, has a disallowed extension, or contains only a dot
// prefix (".env" etc., which the regex would otherwise allow).
func SanitizeAttachmentName(raw string) (string, error) {
	name := filepath.Base(strings.TrimSpace(raw))
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid attachment name")
	}
	if len(name) > AttachmentMaxNameBytes {
		return "", fmt.Errorf("attachment name too long")
	}
	if !attachmentNameRe.MatchString(name) {
		return "", fmt.Errorf("attachment name contains disallowed characters")
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("attachment name must not start with a dot")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !AttachmentAllowedExts[ext] {
		return "", fmt.Errorf("attachment extension %q not allowed", ext)
	}
	return name, nil
}

// resolveUniqueName picks a filename that does not collide with anything
// already inside dir. On collision it appends "-1", "-2", ... before the
// extension, e.g. "image.png" -> "image-1.png".
func resolveUniqueName(dir, name string) (string, error) {
	candidate := name
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		_, err := os.Stat(filepath.Join(dir, candidate))
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
	return "", fmt.Errorf("too many attachment name collisions for %q", name)
}

// dirSizeBytes returns the total size (in bytes) of regular files directly
// under dir. Subdirectories are ignored — attachments are flat by design.
// Missing directory returns 0 without error.
func dirSizeBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// ValidateAttachmentHeaders runs the size + name checks that
// SaveMultipartAttachments would apply, without touching the filesystem. Web
// handlers call this *before* creating the task so a bad upload (oversized,
// bad extension, disallowed name) doesn't leave a half-created task behind.
func ValidateAttachmentHeaders(files []*multipart.FileHeader) error {
	if len(files) == 0 {
		return nil
	}
	var total int64
	for _, fh := range files {
		if fh.Size > AttachmentMaxFileBytes {
			return fmt.Errorf("attachment %q exceeds per-file size cap", fh.Filename)
		}
		if _, err := SanitizeAttachmentName(fh.Filename); err != nil {
			return err
		}
		total += fh.Size
		if total > AttachmentMaxTotalBytes {
			return fmt.Errorf("total attachment size exceeds cap")
		}
	}
	return nil
}

// SaveMultipartAttachments persists every file under the multipart form field
// "attachments" into the task-scoped attachments directory. It enforces:
//   - per-file size cap (AttachmentMaxFileBytes)
//   - aggregate task-dir size cap (AttachmentMaxTotalBytes)
//   - filename sanitization (SanitizeAttachmentName)
//
// On success it returns the list of saved basenames. On any error it removes
// the files it had partially written during this call so the caller can
// surface the error without leaving orphans behind.
func SaveMultipartAttachments(dataHome, taskID string, files []*multipart.FileHeader) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	dir, err := EnsureAttachmentsDir(dataHome, taskID)
	if err != nil {
		return nil, err
	}

	currentTotal, err := dirSizeBytes(dir)
	if err != nil {
		return nil, fmt.Errorf("measure attachments dir: %w", err)
	}

	saved := make([]string, 0, len(files))
	cleanup := func() {
		for _, name := range saved {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}

	for _, fh := range files {
		if fh.Size > AttachmentMaxFileBytes {
			cleanup()
			return nil, fmt.Errorf("attachment %q exceeds per-file size cap", fh.Filename)
		}
		name, err := SanitizeAttachmentName(fh.Filename)
		if err != nil {
			cleanup()
			return nil, err
		}
		name, err = resolveUniqueName(dir, name)
		if err != nil {
			cleanup()
			return nil, err
		}
		if currentTotal+fh.Size > AttachmentMaxTotalBytes {
			cleanup()
			return nil, fmt.Errorf("task attachments would exceed total size cap")
		}

		if err := writeAttachment(filepath.Join(dir, name), fh); err != nil {
			cleanup()
			return nil, err
		}
		currentTotal += fh.Size
		saved = append(saved, name)
	}
	return saved, nil
}

func writeAttachment(path string, fh *multipart.FileHeader) error {
	src, err := fh.Open()
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create attachment: %w", err)
	}
	limited := io.LimitReader(src, AttachmentMaxFileBytes+1)
	n, err := io.Copy(dst, limited)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write attachment: %w", err)
	}
	if n > AttachmentMaxFileBytes {
		_ = os.Remove(path)
		return fmt.Errorf("attachment exceeds size cap during read")
	}
	return nil
}

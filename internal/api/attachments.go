package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// --- Phase 5b PR2 attachments RPCs (docs/plans/phase5-shim-and-task-context.md) ---
// ListAttachments / ReadAttachment back BoidOpTaskAttachmentsList/Get
// (internal/server/boid_executor.go). Both resolve through
// AttachmentsRootForTask — the same directory SaveMultipartAttachments
// writes into and sandbox_builder.go's RO bind exposes — so the RPC reply
// and the parallel file-bind path can never drift apart (wiring-seams.md
// #14).

// ListAttachments returns the basenames of the regular files (and any
// symlink whose resolved target stays inside the directory) directly under
// the task's attachments directory, sorted. Subdirectories are never
// recursed into — attachments are a flat namespace. A task that has never
// received an attachment (missing directory) returns an empty, non-nil
// slice rather than an error, matching the RPC's "no data" convention
// (JSON `[]`, not `null`).
func ListAttachments(dataHome, taskID string) ([]string, error) {
	dir := AttachmentsRootForTask(dataHome, taskID)
	if dir == "" {
		return nil, errors.New("empty attachments path")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue // flat namespace only — no recursion into subdirectories
		}
		info, err := e.Info()
		if err != nil {
			continue // entry vanished mid-scan or is unreadable; skip defensively
		}
		if info.Mode()&os.ModeSymlink != 0 && !symlinkStaysWithin(dir, e.Name()) {
			continue // defense-in-depth: never advertise an escaping symlink
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// ReadAttachment returns the bytes of one attachment, addressed by its
// exact basename. name must be a plain filename with no path separators: a
// traversal or absolute-path attempt ("../../etc/passwd", "/etc/passwd") is
// rejected by validateAttachmentLookupName before any path is constructed,
// rather than being silently coerced into some *other* file the way
// SanitizeAttachmentName's upload-time "pick a safe name to store under"
// contract does. filepath.EvalSymlinks on the final path additionally
// guards against a symlink already sitting in the directory (never created
// by SaveMultipartAttachments' O_CREATE|O_EXCL write, but not something
// this function can assume away on a shared filesystem) being used to
// escape the attachments directory.
func ReadAttachment(dataHome, taskID, name string) ([]byte, error) {
	dir := AttachmentsRootForTask(dataHome, taskID)
	if dir == "" {
		return nil, errors.New("empty attachments path")
	}
	base, err := validateAttachmentLookupName(name)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(dir, base)

	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	if !pathWithinDir(resolvedDir, resolved) {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	if info.Size() > AttachmentMaxFileBytes {
		return nil, fmt.Errorf("attachment %q exceeds size cap", base)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	return data, nil
}

// validateAttachmentLookupName normalizes and validates a caller-supplied
// attachment name for a lookup (list/get RPC). This is deliberately
// stricter than SanitizeAttachmentName's upload-time contract (which uses
// filepath.Base to coerce an arbitrary multipart filename down to a safe
// storage name, discarding any directory components): a lookup must
// address exactly the name the caller asked for, or fail outright — never
// silently resolve to some *other* file's basename.
func validateAttachmentLookupName(name string) (string, error) {
	if name == "" {
		return "", errors.New("attachment name is required")
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("invalid attachment name %q", name)
	}
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid attachment name %q", name)
	}
	cleaned := filepath.Clean(name)
	base := filepath.Base(cleaned)
	if base != name || base == "." {
		return "", fmt.Errorf("invalid attachment name %q", name)
	}
	return base, nil
}

// pathWithinDir reports whether resolved is dir itself or a descendant of
// it. Both arguments must already be fully symlink-resolved (via
// filepath.EvalSymlinks) so this is a pure string-prefix check.
func pathWithinDir(dir, resolved string) bool {
	return resolved == dir || strings.HasPrefix(resolved, dir+string(filepath.Separator))
}

// symlinkStaysWithin reports whether the symlink at dir/name resolves to a
// target inside dir. Used defensively by ListAttachments — normal writes
// never create symlinks, but list must not casually advertise one that
// exists anyway.
func symlinkStaysWithin(dir, name string) bool {
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return pathWithinDir(resolvedDir, resolved)
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

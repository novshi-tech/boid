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
//
// taskID must be a single canonical path component (codex review on PR
// #798, Phase 5b PR2 attachments RPCs): CreateTaskRequest.ID is
// caller-supplied and saved as the literal DB primary key without
// validation (internal/api/task_create.go), and the broker
// (internal/sandbox/broker.go) authorizes attachments ops by comparing that
// *raw* TaskID string against the token's own context — never a resolved
// filesystem path. Without this guard, a task literally IDed
// "alias/../<victim-id>" would pass the broker's string-equality check
// trivially (both sides carry the identical literal alias) while
// filepath.Join here silently collapsed it down to the *victim's* real
// attachments directory — a cross-task leak. Rejecting a non-canonical
// taskID here (returning "", the same fail-closed sentinel already used for
// an empty dataHome/taskID) protects every caller uniformly: the write path
// (EnsureAttachmentsDir, SaveMultipartAttachments) and the read path
// (ListAttachments, ReadAttachment) all resolve through this one function.
func AttachmentsRootForTask(dataHome, taskID string) string {
	if dataHome == "" || taskID == "" {
		return ""
	}
	if !isCanonicalPathComponent(taskID) {
		return ""
	}
	return filepath.Join(dataHome, "tasks", taskID, "attachments")
}

// isCanonicalPathComponent reports whether s is safe to use as a single,
// literal path segment: no path separator, and not a reference to the
// current or parent directory ("." / ".."). Shared by
// AttachmentsRootForTask (taskID) and validateAttachmentLookupName
// (attachment name) — both need the identical "one literal segment, nothing
// clever" contract. Anything that merely *contains* ".." as a substring
// within an otherwise-ordinary segment (e.g. "report..final.png") is safe
// and deliberately allowed: filepath.Clean/Join only treat ".." specially
// when it is a whole path element bounded by separators, never as a
// substring, so a separator-free string can never traverse regardless of
// how many dots it contains.
func isCanonicalPathComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsRune(s, filepath.Separator) {
		return false
	}
	return filepath.Base(filepath.Clean(s)) == s
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
// AttachmentsRootForTask — the same helper SaveMultipartAttachments writes
// through — so the RPC read path and the write path can never drift apart.
// sandbox_builder.go's RO bind, however, builds its source path
// independently (a bare filepath.Join, not this helper — internal/api
// cannot be imported from internal/dispatcher without an import cycle) and
// is validated by a *separate*, deliberately duplicated
// isCanonicalTaskIDComponent (internal/dispatcher/attachments_path.go), not
// isCanonicalPathComponent below directly; see wiring-seams.md #15 for the
// full three-way picture and the residual drift risk this duplication
// leaves (two guard functions that must be kept in lock-step by hand).

// ListAttachments returns the basenames of the regular files directly under
// the task's attachments directory, sorted. Subdirectories are never
// recursed into — attachments are a flat namespace. A task that has never
// received an attachment (missing directory) returns an empty, non-nil
// slice rather than an error, matching the RPC's "no data" convention
// (JSON `[]`, not `null`).
//
// Every symlink is excluded outright, regardless of where it points —
// codex review on PR #798 (Nit) flagged that the original version only
// filtered *escaping* symlinks, which could advertise a name
// ReadAttachment (which now rejects every symlink categorically, via
// openat2 RESOLVE_NO_SYMLINKS on Linux — see the Major/TOCTOU fix) would
// then always 404 on. Non-regular entries (FIFOs, sockets, devices — never
// created by SaveMultipartAttachments, but not something a shared
// filesystem rules out) are excluded for the same reason: list's admission
// criteria must exactly match what get can actually serve.
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
		if !info.Mode().IsRegular() {
			continue // symlink, FIFO, socket, device, ... — get only ever serves regular files
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// ReadAttachment returns the bytes of one attachment, addressed by its
// exact basename. name must be a single canonical path component: a
// traversal or absolute-path attempt ("../../etc/passwd", "/etc/passwd") is
// rejected by validateAttachmentLookupName before any path is constructed,
// rather than being silently coerced into some *other* file the way
// SanitizeAttachmentName's upload-time "pick a safe name to store under"
// contract does. The actual open+read (readAttachmentBytes, platform-
// specific — see attachment_read_linux.go) opens the file exactly once and
// reuses that descriptor for both the containment/type check and the
// capped read, closing the TOCTOU window a separate
// check-then-reopen sequence would leave (codex review on PR #798).
func ReadAttachment(dataHome, taskID, name string) ([]byte, error) {
	dir := AttachmentsRootForTask(dataHome, taskID)
	if dir == "" {
		return nil, errors.New("empty attachments path")
	}
	base, err := validateAttachmentLookupName(name)
	if err != nil {
		return nil, err
	}
	return readAttachmentBytes(dir, base)
}

// readAttachmentFilePortable is the OS-portable attachment reader: the
// Linux fast path (attachment_read_linux.go) falls back to this when the
// running kernel predates openat2 (Linux < 5.6), and any non-Linux build
// uses it unconditionally (attachment_read_other.go) — boid currently
// supports Linux only (CLAUDE.md), so that path exists purely so this
// package still compiles elsewhere.
//
// It re-validates symlink containment via filepath.EvalSymlinks and then
// opens the resolved path ONCE, reusing that same *os.File for both the
// Stat and the size-capped read — closing the "Stat here, ReadFile there"
// half of the TOCTOU codex review flagged (a second path lookup can never
// observe a directory-entry swap that happened after this function's own
// Open call). It cannot close the containment race as completely as
// openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) does — a symlink swap
// landing strictly between EvalSymlinks and this Open is still physically
// possible under adversarial concurrent write access to the directory —
// which is exactly why it is the fallback, not the primary Linux path.
func readAttachmentFilePortable(dir, base string) ([]byte, error) {
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

	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}

	limited := io.LimitReader(f, AttachmentMaxFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	if int64(len(data)) > AttachmentMaxFileBytes {
		return nil, fmt.Errorf("attachment %q exceeds size cap", base)
	}
	return data, nil
}

// validateAttachmentLookupName normalizes and validates a caller-supplied
// attachment name for a lookup (list/get RPC). This is deliberately
// stricter than SanitizeAttachmentName's upload-time contract (which uses
// filepath.Base to coerce an arbitrary multipart filename down to a safe
// storage name, discarding any directory components): a lookup must
// address exactly the name the caller asked for, or fail outright — never
// silently resolve to some *other* file's basename. Shares
// isCanonicalPathComponent with AttachmentsRootForTask's taskID guard, so
// an embedded ".." substring that isn't a whole path element (e.g.
// "report..final.png", which SanitizeAttachmentName already accepts at
// upload time) is allowed here too — codex review on PR #798 flagged the
// previous blanket "contains .." rejection as an unnecessary write/read
// contract mismatch (data present in `list` but unreachable via `get`).
func validateAttachmentLookupName(name string) (string, error) {
	if !isCanonicalPathComponent(name) {
		return "", fmt.Errorf("invalid attachment name %q", name)
	}
	return name, nil
}

// pathWithinDir reports whether resolved is dir itself or a descendant of
// it. Both arguments must already be fully symlink-resolved (via
// filepath.EvalSymlinks) so this is a pure string-prefix check. Used by
// readAttachmentFilePortable's containment check.
func pathWithinDir(dir, resolved string) bool {
	return resolved == dir || strings.HasPrefix(resolved, dir+string(filepath.Separator))
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

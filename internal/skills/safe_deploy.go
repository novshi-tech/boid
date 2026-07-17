package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// This file implements the symlink-attack-resistant primitives DeployAll
// uses to write into baseDir (workspace HOME's `.claude/skills`, per
// internal/dispatcher/runner.go). That directory is rw bind mounted into
// the sandbox for the whole lifetime of every job dispatched against the
// workspace, so every path component under it — including baseDir's own
// skill directories and their subdirectories, not merely the leaf files —
// must be treated as attacker-controlled: a compromised job can replace any
// of them with a symlink to an arbitrary host path between two DeployAll
// calls, or concurrently with one in flight, hoping the daemon (which runs
// as a real, uid 1000 user) writes through it.
//
// A Lstat/EvalSymlinks pre-check cannot close this: a concurrent job can
// swap a real directory for a symlink in the window between the check and
// the subsequent os.MkdirAll/os.CreateTemp/os.Rename call (all of which
// resolve their string path argument fresh, following any symlink they
// find). Every "enter a directory" and "create/replace a file" step below
// instead goes through openat2 with RESOLVE_NO_SYMLINKS (refuse if any
// component being resolved right now is a symlink) — a single syscall that
// checks and opens atomically, so there is no separate check-then-use
// window for a concurrent swap to land in. Once a directory fd is obtained
// this way, later renames of the *name* that led to it cannot affect
// operations already using that fd (Linux resolves fd-relative operations
// against the open file description, not the path).
//
// PR #789 codex review (2026-07-17), Blocker 1.

// openBaseDirSafe opens (creating any missing directory along the way)
// baseDir — an absolute path — verifying that no component of the path, at
// the moment each component is resolved, is a symlink. It walks from the
// filesystem root ("/", which cannot itself be a symlink) so that baseDir's
// own components (not just what's created beneath it) are covered — closing
// the gap the review flagged in the flock-based fallback design
// ("baseDir 自体が symlink な場合には対応できない").
func openBaseDirSafe(baseDir string) (int, error) {
	if !filepath.IsAbs(baseDir) {
		return -1, fmt.Errorf("safe open: path %q must be absolute", baseDir)
	}
	clean := filepath.Clean(baseDir)
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))

	dirFd, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open root: %w", err)
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		childFd, err := openOrCreateDirNoSymlink(dirFd, part)
		_ = unix.Close(dirFd)
		if err != nil {
			return -1, fmt.Errorf("resolving %q at component %q: %w", clean, part, err)
		}
		dirFd = childFd
	}
	return dirFd, nil
}

// openOrCreateDirNoSymlink opens (or creates, if missing) the single path
// component name directly under parentFd, refusing if it turns out to
// currently be a symlink. Every branch that "enters" name — the fast-path
// open, and the retry after creating it — goes through Openat2 with
// RESOLVE_NO_SYMLINKS, so a concurrent symlink swap is always caught at the
// syscall that matters rather than by a preceding check.
func openOrCreateDirNoSymlink(parentFd int, name string) (int, error) {
	how := unix.OpenHow{
		Flags:   unix.O_DIRECTORY | unix.O_RDONLY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
	}
	fd, err := unix.Openat2(parentFd, name, &how)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, classifySafeOpenError(name, err)
	}

	// Doesn't exist yet: create it, then re-resolve through the same
	// symlink-checked path. If a concurrent writer replaced name with a
	// symlink in between, this retry's Openat2 rejects it exactly the same
	// way the fast path would have.
	if mkErr := unix.Mkdirat(parentFd, name, 0o755); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
		return -1, fmt.Errorf("mkdirat %q: %w", name, mkErr)
	}
	fd, err = unix.Openat2(parentFd, name, &how)
	if err != nil {
		return -1, classifySafeOpenError(name, err)
	}
	return fd, nil
}

// classifySafeOpenError turns the symlink-rejection errnos (ELOOP: a
// component was a symlink; EXDEV: RESOLVE_BENEATH would have crossed a
// mount boundary) into a message naming the offending component, per the
// review's "clear error message" requirement, without losing the
// underlying errno for %w-based inspection by callers/tests.
func classifySafeOpenError(name string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("symlink 混入を検出 (component %q): %w", name, err)
	}
	return fmt.Errorf("open %q: %w", name, err)
}

// openFileNoSymlinkIfExists opens name directly under dirFd read-only,
// refusing a symlink, and reporting (nil, false, nil) when it does not
// exist. Used for the "does the existing file already match the embedded
// content" comparison so that read path is symlink-safe too.
func openFileNoSymlinkIfExists(dirFd int, name string) (*os.File, bool, error) {
	how := unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
	}
	fd, err := unix.Openat2(dirFd, name, &how)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, false, nil
		}
		return nil, false, classifySafeOpenError(name, err)
	}
	return os.NewFile(uintptr(fd), name), true, nil
}

// writeFileSafeAt atomically replaces name (a single path component,
// directly under dirFd) with data: write to a sibling temp file, close it,
// renameat it into place (fd-relative on both sides, so neither operand is
// resolved by following any symlink an attacker may have placed at that
// name — POSIX rename never follows the destination's final component even
// without RESOLVE_NO_SYMLINKS, but the source is also fd-relative here for
// consistency and to avoid ever building a path string).
func writeFileSafeAt(dirFd int, name string, data []byte, perm os.FileMode) (retErr error) {
	tmpName, tmp, err := createUniqueTempFile(dirFd, name, perm)
	if err != nil {
		return err
	}
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = unix.Unlinkat(dirFd, tmpName, 0)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %q: %w", tmpName, err)
	}
	if err := unix.Renameat(dirFd, tmpName, dirFd, name); err != nil {
		return fmt.Errorf("rename %q to %q: %w", tmpName, name, err)
	}
	cleanupTemp = false
	return nil
}

// tempFileNameInfix marks writeFileSafeAt's temp file naming convention: a
// dotfile whose name contains ".tmp-".
const tempFileNameInfix = ".tmp-"

// createUniqueTempFile creates a fresh, exclusively-owned (O_EXCL) sibling
// temp file for name directly under dirFd, retrying a handful of times on a
// name collision (astronomically unlikely given the PID+nanosecond+attempt
// suffix, but cheap to guard). O_EXCL guarantees the returned file cannot be
// a symlink an attacker pre-placed — we created it, under a fd we already
// verified is a real directory.
func createUniqueTempFile(dirFd int, name string, perm os.FileMode) (string, *os.File, error) {
	const maxAttempts = 10
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		tmpName := fmt.Sprintf(".%s%s%d-%d-%d", name, tempFileNameInfix, os.Getpid(), time.Now().UnixNano(), i)
		fd, err := unix.Openat(dirFd, tmpName, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, uint32(perm))
		if err == nil {
			return tmpName, os.NewFile(uintptr(fd), tmpName), nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", nil, fmt.Errorf("create temp file %q: %w", tmpName, err)
		}
		lastErr = err
	}
	return "", nil, fmt.Errorf("create temp file for %q: exhausted %d attempts: %w", name, maxAttempts, lastErr)
}

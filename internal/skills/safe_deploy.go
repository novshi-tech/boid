package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// This file implements the symlink-attack-resistant primitives this package
// uses to create directories and write files under a path the DAEMON walks
// but a JOB controls.
//
// MkdirAllNoSymlink is the current concrete case: the dispatcher calls it
// to create the per-skill bind TARGETS inside the workspace HOME, which is
// rw bind mounted into the sandbox for the whole lifetime of every job
// dispatched against that workspace. Every path component under such a
// directory — not merely the leaf files — has to be treated as
// attacker-controlled: a compromised job can replace any of them with a
// symlink to an arbitrary host path between two calls, or concurrently with
// one in flight, hoping the daemon (which runs as a real, uid 1000 user)
// writes through it.
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

// MkdirAllNoSymlink creates dir and every missing component leading to it,
// using the same symlink-refusing openat2 walk DeployAll writes through (see
// this file's package comment for the threat model), and reports the uid that
// owns the resulting directory. dir must be absolute; creating an
// already-existing directory is a no-op.
//
// The dispatcher calls this to pre-create the per-skill bind TARGETS
// (<home>/.claude and <home>/.claude/skills/<name>) inside the workspace
// HOME before bind-mounting each embedded skill in from a host-visible
// runtime dir. A missing bind target would otherwise be auto-created by the
// container engine as uid 0, which locks the uid 1000 harness out of
// ~/.claude/.credentials.json.
//
// Those targets sit inside a directory the job owns read-write, which is
// precisely why this is not an os.MkdirAll: a job that leaves ~/.claude
// behind as a symlink must not turn the next dispatch's mkdir into a write
// somewhere else on the daemon's filesystem.
//
// The returned uid answers the question a bare mkdir cannot: "is the directory
// now at this path one I own, or one somebody else created first?" A caller
// that pre-creates a bind target inside job-owned storage needs that — an
// EEXIST is indistinguishable from success otherwise, and a target already
// created by the engine as uid 0 is not repairable afterwards (see
// internal/dispatcher/skills_overlay.go, the caller that acts on it, for why
// that is the outcome worth failing on). It is deliberately a plain uid rather
// than a bool: this package cannot know which uid its caller considers its
// own, and hard-coding one (1000) would be wrong on any deploy that runs the
// daemon as some other user.
//
// It is read with fstat(2) on the descriptor the walk ended on, never a stat
// by path. A stat by path would re-resolve every component from scratch, so it
// could report the owner of a directory other than the one just created or
// entered — exactly the check-then-use gap this file's openat2 design exists
// to remove. Doing the ownership check through such a gap would make the check
// itself the race.
//
// No separate "is it a directory" result is returned because there is nothing
// left to report: every component of the walk is opened with O_DIRECTORY (see
// openOrCreateDirNoSymlink), so a non-directory at any position — a plain file
// left at ~/.claude, say — fails the call outright with ENOTDIR rather than
// reaching this return. internal/dispatcher's
// TestDispatch_SkillsBindTargetPrepFails_MarksJobFailedAndCallsCleanup pins
// that end of it.
func MkdirAllNoSymlink(dir string) (ownerUID int, err error) {
	fd, err := openBaseDirSafe(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = unix.Close(fd) }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, fmt.Errorf("fstat %q: %w", dir, err)
	}
	return int(st.Uid), nil
}

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
// mount boundary) into a message naming the offending component, without
// losing the underlying errno for %w-based inspection by callers/tests.
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
// directly under dirFd) with data: write to a sibling temp file, fsync it,
// close it, renameat it into place (fd-relative on both sides, so neither
// operand is resolved by following any symlink an attacker may have placed
// at that name — POSIX rename never follows the destination's final
// component even without RESOLVE_NO_SYMLINKS, but the source is also
// fd-relative here for consistency and to avoid ever building a path
// string), then fsync dirFd itself so the rename is durable across a crash
// right after this call returns (mirrors
// internal/dispatcher/workspace_home.go's writeWorkspaceHomeMarker's temp ->
// sync -> close -> rename -> parent-dir-sync pattern). Without the two Sync
// calls, a SIGKILL or power loss between write and rename can leave dest
// holding a partially written file or, on some filesystems/journaling
// modes, an unlinked rename that never made it to disk.
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %q: %w", tmpName, err)
	}
	if err := unix.Renameat(dirFd, tmpName, dirFd, name); err != nil {
		return fmt.Errorf("rename %q to %q: %w", tmpName, name, err)
	}
	cleanupTemp = false

	// Best-effort: not fatal if the underlying filesystem doesn't support
	// fsync on a directory fd.
	_ = unix.Fsync(dirFd)
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

// isStaleTempName reports whether name looks like a leftover atomic-write
// temp file (createUniqueTempFile's naming convention: a dotfile containing
// ".tmp-"), matching the pattern assertNoTempFiles in deploy_test.go already
// pins for "no leftovers after a normal run".
func isStaleTempName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, tempFileNameInfix)
}

// cleanupStaleTempFiles removes any stale atomic-write temp file directly
// under dirFd. This is the recovery half of the crash-safety contract
// writeFileSafeAt's Sync calls only cover the other half of: a daemon
// killed (SIGKILL, power loss) between createUniqueTempFile and the
// renameat leaves a temp file behind forever otherwise, since a deferred
// unlinkat never runs on that code path. Called once per directory at the
// start of deploySkillDir, before any new writes, so every dispatch's
// DeployAll call reclaims whatever a previous crashed run left behind — via
// the same symlink-safe (unlinkat on a verified fd, not a path string)
// mechanism as the rest of this file.
//
// A name matching isStaleTempName is not necessarily abandoned: two calls
// (e.g. two concurrent `boid install-skills` runs against the same
// ~/.claude/skills) can legitimately run at once against the same baseDir,
// and unlinking every matching name unconditionally would delete a
// sibling's temp file moments before its own renameat, producing a spurious
// "rename ... no such file or directory" failure. tempFileOwnerAlive
// distinguishes a genuinely abandoned temp file (its creating PID, encoded
// in the name by createUniqueTempFile, no longer exists) from one whose
// owner is still alive and presumably still writing it — only the former
// is reaped here.
func cleanupStaleTempFiles(dirFd int) error {
	dupFd, err := unix.Dup(dirFd)
	if err != nil {
		return fmt.Errorf("dup dir fd: %w", err)
	}
	f := os.NewFile(uintptr(dupFd), "skill-dir")
	defer f.Close()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("readdir: %w", err)
	}
	for _, n := range names {
		if !isStaleTempName(n) {
			continue
		}
		if tempFileOwnerAlive(n) {
			continue
		}
		if err := unix.Unlinkat(dirFd, n, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unlink stale temp file %q: %w", n, err)
		}
	}
	return nil
}

// tempFileOwnerAlive reports whether name — a temp file name matching
// isStaleTempName — was created by a process that is still running,
// based on the PID createUniqueTempFile encodes as the first "-"-separated
// component after tempFileNameInfix (".<name>.tmp-<pid>-<nanotime>-<attempt>").
// A name whose PID component cannot be parsed as a positive integer (e.g.
// TestDeployAll_CleansUpStaleTempFiles' synthetic
// ".SKILL.md.tmp-stale-12345" fixture, predating this function) is treated
// as having no identifiable owner and therefore reapable — this keeps the
// original "clean up anything that merely looks stale" contract as the
// fallback for names this parse can't make sense of, and only withholds
// reaping when a live owner is positively identified.
func tempFileOwnerAlive(name string) bool {
	idx := strings.Index(name, tempFileNameInfix)
	if idx < 0 {
		return false
	}
	rest := name[idx+len(tempFileNameInfix):]
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) != 3 {
		return false
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// processAlive reports whether pid identifies a currently-running process,
// via the standard "kill -0" liveness idiom: sending signal 0 performs
// every permission/existence check a real signal delivery would without
// actually delivering one. ESRCH ("no such process") is the only outcome
// treated as dead; EPERM (the process exists but this daemon lacks
// permission to signal it — cannot happen for a temp file this same uid
// created, but handled for defense in depth) and any other error are
// treated as "cannot prove it's dead", i.e. alive — reaping is the
// destructive direction here, so an inconclusive check must not reap.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, unix.ESRCH)
}

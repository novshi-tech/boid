package dispatcher

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// workspace_home_tar.go is the HOST-side half of PR8 of
// docs/plans/workspace-home-volume-persistence.md (論点 f): it turns a pre-PR6
// workspace home directory — ~/.local/share/boid/homes/<slug>, the layout
// WorkspaceHomesDir still resolves — into the byte stream
// `boid workspace import-home` streams to the daemon, and the daemon feeds to
// an extraction container's stdin.
//
// # Why a tar stream rather than a bind mount (論点 f)
//
// The obvious implementation is to bind the legacy directory into a container
// and `cp -a` it onto the volume. 論点 f rejects that, and the reason is the
// incident the whole volume-only pivot came out of
// (docs/plans/volume-only-daemon.md's 「壁 2」): under rootless podman the
// container's uid 1000 is NOT the host's uid 1000, so a bind of a directory
// holding a 0600 .credentials.json is unreadable from inside — and running as
// container uid 0 instead reads it but then writes everything into the volume
// owned by a uid the harness is not. A tar stream crosses that boundary as
// DATA: this process reads the files as the host user who owns them, and the
// extraction re-creates them as whatever uid the job container runs as
// (see workspaceHomeImportArgv).
//
// # This code runs in the CLI, not the daemon
//
// It lives in internal/dispatcher because that is where WorkspaceHomesDir lives
// and where its doc comment already promises PR8 will read from it — not
// because the daemon calls it. The daemon under the compose deploy cannot see
// the host's filesystem at all, which is exactly why the tar is produced on the
// host side and streamed (see Runner.ImportWorkspaceHome's D1 note).

// WorkspaceHomeTarStats is what a walk produced (and, while it is running, how
// far it has got). Reported both incrementally through WriteWorkspaceHomeTar's
// progress callback and once at the end as its return value.
type WorkspaceHomeTarStats struct {
	// Files is the number of regular-file entries written, hard-linked
	// duplicates included.
	Files int64
	// Dirs is the number of directory entries written, excluding the root.
	Dirs int64
	// Symlinks is the number of symlink entries written.
	Symlinks int64
	// HardLinks is how many entries were written as a link to a name already in
	// the archive rather than as another copy of the payload.
	HardLinks int64
	// Bytes is the total file content written — a hard-linked file's payload
	// counted ONCE, matching how the engine's own `docker system df` sizes a
	// volume (see WorkspaceHomeVolumeStore's doc comment). It is the number to
	// compare against a `boid workspace show` size, not the archive's length.
	Bytes int64
	// Skipped names every entry the walk deliberately left out, with the reason
	// — see the irregular-file branch in writeWorkspaceHomeEntry. Reported
	// rather than logged because a migration that silently dropped part of a
	// home is indistinguishable from one that copied all of it.
	Skipped []string
}

// workspaceHomeTarProgressInterval bounds how often the progress callback
// fires. Generous because the callback is a terminal write on a walk that can
// touch hundreds of thousands of files: fast enough that a 4.3GB transfer never
// looks hung (D4's 「進捗が見えること」), slow enough not to become the walk's
// own bottleneck.
const workspaceHomeTarProgressInterval = 2 * time.Second

// WriteWorkspaceHomeTar writes root's contents to w as an uncompressed tar
// stream, and reports what it wrote.
//
// progress, when non-nil, is called at most every
// workspaceHomeTarProgressInterval with the stats so far, and once more is NOT
// implied at the end — the return value is the final figure.
//
// # What crosses, and what deliberately does not
//
//   - Modes cross exactly (D3: 「`0600` などの mode は維持すること」). The
//     extraction side is what makes that stick; see workspaceHomeImportArgv.
//   - Ownership does NOT cross. Every entry is written uid/gid 0 with no
//     user/group names, because the extraction discards ownership anyway
//     (--no-same-owner) and recording the host account into an artifact whose
//     whole purpose is to cross a uid-mapping boundary is noise at best.
//   - Symlinks INSIDE the tree cross as symlinks, targets verbatim — absolute
//     ones included. Following them would duplicate payloads and flatten a
//     toolchain's internal structure; rewriting them would be boid inventing a
//     target the workspace never had. An absolute symlink into the host
//     filesystem is already broken inside the sandbox and stays broken, which is
//     the honest outcome (memory: 「workspace HOME 配下の symlink は relative
//     必須」).
//   - The ROOT is the one exception: a root that is itself a symlink is
//     RESOLVED and its target walked. The asymmetry is deliberate — the root is
//     the directory the operator named on the command line, while the links
//     inside it are the home's own structure. See ResolveWorkspaceHomeSource for
//     the empty-archive bug that made this explicit.
//   - Hard links cross as links. A node/volta toolchain hard-links
//     aggressively, and writing each name's content again would inflate a
//     multi-GB transfer for nothing.
//   - Sockets, FIFOs and device nodes are SKIPPED and named in Skipped. None of
//     them is state a workspace home needs restored (an agent's IPC socket is
//     re-created on demand), and a FIFO in particular would make the extraction
//     block or fail.
//   - Sparse files are read densely: a 100MiB sparse file crosses as 100MiB.
//     archive/tar has no portable sparse encoding worth relying on, and the
//     alternative — dropping the file — loses data.
//
// It never writes to root. That is D4's first rule (「source は絶対に削除・変更
// しない。読むだけ」) and the reason the migration is safe to re-run.
func WriteWorkspaceHomeTar(w io.Writer, root string, progress func(WorkspaceHomeTarStats)) (WorkspaceHomeTarStats, error) {
	var stats WorkspaceHomeTarStats

	// Resolved rather than merely validated — see ResolveWorkspaceHomeSource for
	// the bug this closes. The walk below MUST run against the resolved path:
	// filepath.WalkDir does not follow a symlinked root, and the `path == root`
	// branch then skips the only entry it produces, which is how a --from
	// pointing at a symlink used to yield a valid, empty, successful archive.
	root, err := ResolveWorkspaceHomeSource(root)
	if err != nil {
		return stats, err
	}

	tw := tar.NewWriter(w)
	// seen maps a (device, inode) of a multiply-linked file to the archive name
	// that already carries its payload.
	seen := map[[2]uint64]string{}
	lastReport := time.Now()

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", path, err)
		}
		if path == root {
			// The root itself is deliberately not an entry. Extraction targets
			// the volume's mount point, and a "./" entry would apply the legacy
			// directory's own mode to it.
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize %q: %w", path, err)
		}
		if err := writeWorkspaceHomeEntry(tw, path, rel, d, seen, &stats); err != nil {
			return err
		}
		if progress != nil && time.Since(lastReport) >= workspaceHomeTarProgressInterval {
			lastReport = time.Now()
			progress(stats)
		}
		return nil
	})
	if walkErr != nil {
		// The writer is closed but its error is discarded: the walk's error is
		// the one that describes what went wrong, and a Close error on a stream
		// that is already being abandoned would only mask it.
		_ = tw.Close()
		return stats, walkErr
	}
	if err := tw.Close(); err != nil {
		return stats, fmt.Errorf("finish workspace home archive: %w", err)
	}
	if progress != nil {
		progress(stats)
	}
	return stats, nil
}

// writeWorkspaceHomeEntry writes one filesystem object as one tar entry.
func writeWorkspaceHomeEntry(tw *tar.Writer, path, rel string, d fs.DirEntry, seen map[[2]uint64]string, stats *WorkspaceHomeTarStats) error {
	// Lstat, not d.Info() — they agree today (WalkDir does not follow symlinks)
	// but the distinction is the one that decides whether a symlink is carried
	// or followed, so it is spelled out.
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}

	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read symlink %q: %w", path, err)
		}
		hdr, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return fmt.Errorf("header for %q: %w", path, err)
		}
		normalizeWorkspaceHomeHeader(hdr, rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header for %q: %w", path, err)
		}
		stats.Symlinks++
		return nil

	case mode.IsDir():
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("header for %q: %w", path, err)
		}
		normalizeWorkspaceHomeHeader(hdr, rel+"/")
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header for %q: %w", path, err)
		}
		stats.Dirs++
		return nil

	case mode.IsRegular():
		return writeWorkspaceHomeFile(tw, path, rel, info, seen, stats)

	default:
		// Socket, FIFO, device, or anything else the kernel grew since. Named
		// rather than silently dropped. This arm is the complement of
		// workspaceHomeTarCarries — keep them in step, since the pre-flight that
		// refuses an empty migration source decides emptiness with that
		// predicate.
		stats.Skipped = append(stats.Skipped, fmt.Sprintf("%s (%s)", rel, describeIrregularMode(mode)))
		return nil
	}
}

// writeWorkspaceHomeFile writes a regular file, as a hard link when its payload
// is already in the archive under another name.
func writeWorkspaceHomeFile(tw *tar.Writer, path, rel string, info os.FileInfo, seen map[[2]uint64]string, stats *WorkspaceHomeTarStats) error {
	if key, multiplyLinked := hardLinkKey(info); multiplyLinked {
		if first, ok := seen[key]; ok {
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("header for %q: %w", path, err)
			}
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = first
			hdr.Size = 0
			normalizeWorkspaceHomeHeader(hdr, rel)
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write hard-link header for %q: %w", path, err)
			}
			stats.HardLinks++
			stats.Files++
			return nil
		}
		seen[key] = rel
	}

	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("header for %q: %w", path, err)
	}
	normalizeWorkspaceHomeHeader(hdr, rel)
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header for %q: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	// io.CopyN with the header's size rather than io.Copy: a file that GREW
	// between the Lstat and this read would otherwise overrun the entry and
	// corrupt every entry after it. A file that SHRANK is caught by CopyN's own
	// ErrUnexpectedEOF. Either way the migration fails loudly instead of
	// producing an archive that extracts into garbage — which matters because
	// the daemon has already destroyed the volume by the time it reads this.
	n, err := io.CopyN(tw, f, hdr.Size)
	if err != nil {
		return fmt.Errorf("read %q (%d of %d bytes; the file changed while it was being archived): %w", path, n, hdr.Size, err)
	}
	stats.Files++
	stats.Bytes += n
	return nil
}

// normalizeWorkspaceHomeHeader applies the two rules every entry obeys: the
// name is the slash-separated path relative to the home root, and ownership is
// dropped.
//
// filepath.ToSlash is not cosmetic even on a Linux-only project: tar names are
// defined as slash-separated, and going through it makes that a property of the
// format rather than a coincidence of the platform.
func normalizeWorkspaceHomeHeader(hdr *tar.Header, name string) {
	hdr.Name = filepath.ToSlash(name)
	hdr.Uid = 0
	hdr.Gid = 0
	hdr.Uname = ""
	hdr.Gname = ""
	// Format is left to archive/tar's own choice (PAX when a field needs it,
	// USTAR otherwise): GNU tar reads both, and pinning one here would silently
	// truncate long names or sub-second timestamps.
}

// hardLinkKey reports the (device, inode) identity of a file that has more than
// one name, and whether it has one at all.
//
// A file with Nlink == 1 is not tracked: the map would then grow an entry per
// file in a home with hundreds of thousands of them, for a lookup that can
// never hit.
func hardLinkKey(info os.FileInfo) ([2]uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return [2]uint64{}, false
	}
	return [2]uint64{uint64(st.Dev), st.Ino}, true
}

// describeIrregularMode names why an entry was skipped, in words an operator
// can act on.
func describeIrregularMode(mode os.FileMode) string {
	switch {
	case mode&os.ModeSocket != 0:
		return "unix socket, not restorable"
	case mode&os.ModeNamedPipe != 0:
		return "named pipe, not restorable"
	case mode&os.ModeDevice != 0:
		return "device node, not restorable"
	case mode&os.ModeCharDevice != 0:
		return "character device, not restorable"
	default:
		return "irregular file, not restorable"
	}
}

// ResolveWorkspaceHomeSource validates root as a migration source and returns
// the path a walk of it has to actually run against.
//
// # Why this exists (codex review of PR8 round 3, Blocker 2)
//
// Every pre-flight in this feature used os.Stat, which FOLLOWS a symlink and
// answers "yes, a directory" for `homes/team-a -> /mnt/backup/team-a`. The walk
// disagrees: filepath.WalkDir lstats its root, sees a symlink rather than a
// directory, does not descend, and hands the callback exactly one path — the
// root — which WriteWorkspaceHomeTar skips unconditionally. The result is a
// well-formed archive with ZERO entries and no error anywhere, and since the
// daemon destroys the workspace's HOME volume before it reads a byte of the
// body, that archive extracts cleanly over the wreckage and the CLI prints
// "imported".
//
// # Resolved rather than refused
//
// Refusing a symlinked --from would also be safe, and it would be worse: a
// symlink is how an operator points at a home that lives on another disk, and
// "we followed the link you gave us" is what every other tool they use does. The
// cost of following it is bounded — this is a READ of a directory the operator
// named, and the archive it produces is the same one a --from of the target
// would produce.
//
// Only the FINAL component is resolved, and only when it is itself a link. An
// intermediate symlink needs no special handling (WalkDir lstats the root, which
// is a directory, and descends normally), and resolving one would only make the
// path in the confirmation prompt less like the one the operator typed.
//
// Links INSIDE the tree are untouched and still cross as links — see
// WriteWorkspaceHomeTar's contract for why that asymmetry is deliberate: the
// root is the thing the operator NAMED, while the links inside it are the
// structure of the home itself.
func ResolveWorkspaceHomeSource(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("read workspace home source %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.IsDir() {
			return "", fmt.Errorf("workspace home source %q is not a directory", root)
		}
		return root, nil
	}

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace home source %q is a symlink that does not resolve: %w", root, err)
	}
	target, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("read workspace home source %q (a symlink to %q): %w", root, resolved, err)
	}
	if !target.IsDir() {
		return "", fmt.Errorf("workspace home source %q is a symlink to %q, which is not a directory", root, resolved)
	}
	return resolved, nil
}

// WorkspaceHomeSourceHasEntries reports whether root holds anything
// WriteWorkspaceHomeTar would put in the archive.
//
// # Why a separate question (codex round 3, Blocker 2, second half)
//
// The symlinked root was ONE way to produce an archive with no entries. It is
// not the only one: a --from with a typo in it, a backup directory that was
// never populated, a mount that is not mounted, all land in the same place — and
// that place is uniquely destructive, because the daemon removes the HOME volume
// before it reads the body. So the outcome is refused on its own terms rather
// than only its known causes, and it is refused on BOTH sides of the wire (see
// Runner.ImportWorkspaceHome's own check, which is the one that sits on the same
// side as the deletion).
//
// Refusing an empty source costs nothing an operator needs: "make this
// workspace's home empty" is `boid workspace remove` plus a re-create, which
// says what it means and does not run through a migration command.
//
// # Cost
//
// It stops at the first entry that would be carried (fs.SkipAll), so on a real
// home it is one readdir. The pathological input — a directory holding only
// sockets and FIFOs — is the only one that walks the whole tree, and it is
// exactly the input whose answer cannot be short-circuited.
func WorkspaceHomeSourceHasEntries(root string) (bool, error) {
	resolved, err := ResolveWorkspaceHomeSource(root)
	if err != nil {
		return false, err
	}

	found := false
	walkErr := filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", path, err)
		}
		if path == resolved {
			// Skipped for the same reason the writer skips it: the root is not an
			// entry, so a home consisting of nothing but itself carries nothing.
			return nil
		}
		info, lerr := os.Lstat(path)
		if lerr != nil {
			return fmt.Errorf("stat %q: %w", path, lerr)
		}
		if workspaceHomeTarCarries(info.Mode()) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return false, walkErr
	}
	return found, nil
}

// workspaceHomeTarCarries reports whether writeWorkspaceHomeEntry turns a file
// of this mode into a tar entry rather than adding it to Skipped.
//
// It exists so the pre-flight above and the writer cannot answer that question
// differently: a pre-flight that counted an entry the writer drops would let an
// empty archive through, and one that dropped an entry the writer carries would
// refuse a migration that had something to move. The writer's own switch is kept
// as a switch because each branch does different work; its default arm is
// exactly the complement of this predicate, and the FIFO case is pinned on both
// sides (TestWriteWorkspaceHomeTar_SkipsIrregularFilesAndReportsThem,
// TestWorkspaceHomeSourceHasEntries).
func workspaceHomeTarCarries(mode os.FileMode) bool {
	return mode&os.ModeSymlink != 0 || mode.IsDir() || mode.IsRegular()
}

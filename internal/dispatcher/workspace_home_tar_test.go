package dispatcher

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// WriteWorkspaceHomeTar is the HOST-side half of PR8 (論点 f): it turns a
// pre-PR6 workspace home directory into the byte stream the daemon feeds an
// extraction container's stdin.
//
// Everything here is about the properties that make the round trip lossless in
// the ways that matter (modes, symlinks, hardlinks) and safe in the ways that
// matter (nothing absolute, nothing above the root, the source never touched).

func readTarEntries(t *testing.T, data []byte) map[string]*tar.Header {
	t.Helper()
	entries := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		entries[h.Name] = h
	}
	return entries
}

func TestWriteWorkspaceHomeTar_PreservesModes(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, ".claude", ".credentials.json"), "secret", 0o600)
	mustWrite(t, filepath.Join(src, "bin", "tool"), "#!/bin/sh\n", 0o755)
	if err := os.Chmod(filepath.Join(src, ".claude"), 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, src, nil); err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	entries := readTarEntries(t, buf.Bytes())

	for name, want := range map[string]int64{
		".claude/.credentials.json": 0o600,
		"bin/tool":                  0o755,
		".claude/":                  0o700,
	} {
		h, ok := entries[name]
		if !ok {
			t.Errorf("no entry %q in %v", name, tarEntryNames(entries))
			continue
		}
		if h.Mode&0o7777 != want {
			t.Errorf("%s mode = %04o, want %04o — a credentials file whose permissions loosen in transit is a regression",
				name, h.Mode&0o7777, want)
		}
	}
}

func TestWriteWorkspaceHomeTar_NormalizesOwnership(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a"), "x", 0o644)

	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, src, nil); err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	h := readTarEntries(t, buf.Bytes())["a"]
	if h == nil {
		t.Fatal("no entry")
	}
	if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
		t.Errorf("entry carries ownership uid=%d gid=%d uname=%q gname=%q; the extraction discards it (--no-same-owner), "+
			"so recording it only leaks the host account into an artifact that crosses a uid-mapping boundary",
			h.Uid, h.Gid, h.Uname, h.Gname)
	}
}

func TestWriteWorkspaceHomeTar_CarriesSymlinksAsLinks(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real"), "x", 0o644)
	if err := os.Symlink("real", filepath.Join(src, "rel-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "abs-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, src, nil); err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	entries := readTarEntries(t, buf.Bytes())

	link := entries["rel-link"]
	if link == nil || link.Typeflag != tar.TypeSymlink || link.Linkname != "real" {
		t.Errorf("rel-link = %+v, want a symlink entry pointing at %q (following it instead would duplicate the payload "+
			"and, for a link into a toolchain, silently flatten it)", link, "real")
	}
	// An absolute symlink is carried VERBATIM rather than resolved or dropped:
	// it is broken inside the sandbox either way (the daemon does not have the
	// host's filesystem), and rewriting one here would be boid inventing a
	// target the workspace never had.
	abs := entries["abs-link"]
	if abs == nil || abs.Typeflag != tar.TypeSymlink || abs.Linkname != "/etc/passwd" {
		t.Errorf("abs-link = %+v, want the absolute target carried verbatim", abs)
	}
}

func TestWriteWorkspaceHomeTar_DeduplicatesHardLinks(t *testing.T) {
	src := t.TempDir()
	payload := strings.Repeat("x", 4096)
	mustWrite(t, filepath.Join(src, "node_modules", "pkg", "bin.js"), payload, 0o644)
	if err := os.Link(filepath.Join(src, "node_modules", "pkg", "bin.js"), filepath.Join(src, "node_modules", "pkg", "alias.js")); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	var buf bytes.Buffer
	stats, err := WriteWorkspaceHomeTar(&buf, src, nil)
	if err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	entries := readTarEntries(t, buf.Bytes())

	var linkCount, regCount int
	for _, h := range entries {
		switch h.Typeflag {
		case tar.TypeLink:
			linkCount++
		case tar.TypeReg:
			regCount++
		}
	}
	if linkCount != 1 || regCount != 1 {
		t.Errorf("hard-link pair encoded as %d link + %d regular entries, want 1 + 1: node/volta toolchains hard-link "+
			"aggressively, and writing each name's content again inflates a multi-GB transfer for nothing", linkCount, regCount)
	}
	if stats.HardLinks != 1 {
		t.Errorf("stats.HardLinks = %d, want 1", stats.HardLinks)
	}
	if stats.Bytes != int64(len(payload)) {
		t.Errorf("stats.Bytes = %d, want %d (the payload counted once)", stats.Bytes, len(payload))
	}
}

func TestWriteWorkspaceHomeTar_SkipsIrregularFilesAndReportsThem(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "keep"), "x", 0o644)
	fifo := filepath.Join(src, "fifo")
	if err := mkfifoForTest(fifo); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	var buf bytes.Buffer
	stats, err := WriteWorkspaceHomeTar(&buf, src, nil)
	if err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	entries := readTarEntries(t, buf.Bytes())
	if _, ok := entries["fifo"]; ok {
		t.Error("a FIFO was written into the archive; extracting it would block or fail in the container")
	}
	if _, ok := entries["keep"]; !ok {
		t.Error("an irregular file aborted the whole walk")
	}
	if len(stats.Skipped) != 1 || !strings.Contains(stats.Skipped[0], "fifo") {
		t.Errorf("stats.Skipped = %v, want the FIFO named: a silently dropped file is how a migration reports success "+
			"while losing something", stats.Skipped)
	}
}

func TestWriteWorkspaceHomeTar_LeavesTheSourceUntouched(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, ".claude", ".credentials.json")
	mustWrite(t, path, "secret", 0o600)
	// A distinctive mtime, so "untouched" covers atime-driven metadata writes
	// as well as content.
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, src, nil); err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() || after.Size() != before.Size() {
		t.Errorf("the source changed: before=%v/%v/%d after=%v/%v/%d — the migration READS the legacy home and must never "+
			"write to it (D4)", before.ModTime(), before.Mode(), before.Size(), after.ModTime(), after.Mode(), after.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "secret" {
		t.Errorf("source content = %q (err %v), want %q", data, err, "secret")
	}
}

func TestWriteWorkspaceHomeTar_EntryNamesAreRelativeAndContained(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a", "b", "c"), "x", 0o644)

	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, src, nil); err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	for name := range readTarEntries(t, buf.Bytes()) {
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || name == ".." || strings.Contains(name, "/../") {
			t.Errorf("entry %q escapes the archive root; tar would write it outside the workspace home", name)
		}
		if name == "./" || name == "." {
			t.Errorf("entry %q re-states the root; extraction would chmod the volume's own mount point", name)
		}
	}
}

// TestWriteWorkspaceHomeTar_ResolvesASymlinkedRoot is codex round 3's Blocker 2,
// first half.
//
// `homes/team-a -> /mnt/backup/team-a` is an entirely ordinary thing for an
// operator to have, and every pre-flight check in this feature used os.Stat,
// which FOLLOWS the link and answers "yes, a directory". filepath.WalkDir does
// not follow it: the root's fs.DirEntry reports a symlink, so the walk never
// descends, the callback's `path == root` branch skips the one entry it does get,
// and the archive comes out with zero entries and no error.
//
// That combination is the worst one this feature can produce. The daemon destroys
// the workspace's HOME volume before it reads a byte of the body, so a successful
// empty archive extracts into an empty volume, returns 200, and the CLI prints
// "imported" over a home that now holds nothing.
func TestWriteWorkspaceHomeTar_ResolvesASymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	mustWrite(t, filepath.Join(real, ".claude", ".credentials.json"), "secret", 0o600)
	mustWrite(t, filepath.Join(real, "bin", "tool"), "#!/bin/sh\n", 0o755)

	link := filepath.Join(t.TempDir(), "team-a")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var buf bytes.Buffer
	stats, err := WriteWorkspaceHomeTar(&buf, link, nil)
	if err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	if stats.Files != 2 {
		t.Fatalf("stats.Files = %d, want 2: a --from that is a symlink to the real home archived NOTHING, and the daemon "+
			"had already destroyed the workspace's HOME volume by the time it read this stream", stats.Files)
	}
	entries := readTarEntries(t, buf.Bytes())
	for _, name := range []string{".claude/.credentials.json", "bin/tool"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("no entry %q in %v", name, tarEntryNames(entries))
		}
	}
	// Only the ROOT is resolved. Links INSIDE the home stay links — that is the
	// structure of a toolchain and rewriting it would be boid inventing targets
	// the workspace never had (see this file's other symlink test).
	if stats.Symlinks != 0 {
		t.Errorf("stats.Symlinks = %d, want 0: nothing inside this source is a link", stats.Symlinks)
	}
}

// TestWorkspaceHomeSourceHasEntries is Blocker 2's second half: the defence that
// does not depend on having predicted the reason.
//
// A source that yields no entries reaches the daemon as an archive that extracts
// cleanly into the volume it just destroyed. The symlinked root was one way to
// get there; a --from pointed at an empty directory (a typo, a backup that was
// never populated, a mount that is not mounted) is another, and there will be
// more. So "nothing would be migrated" is refused on its own terms, before
// anything is sent.
func TestWorkspaceHomeSourceHasEntries(t *testing.T) {
	empty := t.TempDir()
	if has, err := WorkspaceHomeSourceHasEntries(empty); err != nil || has {
		t.Errorf("WorkspaceHomeSourceHasEntries(<empty dir>) = %v, %v; want false, nil — sending this archive destroys the "+
			"workspace's home and replaces it with nothing", has, err)
	}

	populated := t.TempDir()
	mustWrite(t, filepath.Join(populated, ".claude", ".credentials.json"), "secret", 0o600)
	if has, err := WorkspaceHomeSourceHasEntries(populated); err != nil || !has {
		t.Errorf("WorkspaceHomeSourceHasEntries(<populated dir>) = %v, %v; want true, nil", has, err)
	}

	// A directory is an entry in its own right: an empty ~/.claude still crosses,
	// and refusing that source would be refusing a migration that has something
	// to say.
	dirsOnly := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirsOnly, ".claude"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if has, err := WorkspaceHomeSourceHasEntries(dirsOnly); err != nil || !has {
		t.Errorf("WorkspaceHomeSourceHasEntries(<dirs only>) = %v, %v; want true, nil", has, err)
	}

	// It agrees with the writer about what a symlinked root means, or the CLI
	// would refuse exactly the source the writer handles correctly.
	link := filepath.Join(t.TempDir(), "team-a")
	if err := os.Symlink(populated, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if has, err := WorkspaceHomeSourceHasEntries(link); err != nil || !has {
		t.Errorf("WorkspaceHomeSourceHasEntries(<symlink to populated dir>) = %v, %v; want true, nil", has, err)
	}

	// And a source that holds ONLY things the writer skips counts as empty,
	// because the archive it produces is empty.
	socketsOnly := t.TempDir()
	if err := mkfifoForTest(filepath.Join(socketsOnly, "fifo")); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	if has, err := WorkspaceHomeSourceHasEntries(socketsOnly); err != nil || has {
		t.Errorf("WorkspaceHomeSourceHasEntries(<only irregular files>) = %v, %v; want false, nil — every one of those is "+
			"skipped, so the archive carries nothing", has, err)
	}
}

func TestWriteWorkspaceHomeTar_RejectsAMissingOrNonDirectorySource(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteWorkspaceHomeTar(&buf, filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Error("a missing source produced an archive rather than an error")
	}
	file := filepath.Join(t.TempDir(), "f")
	mustWrite(t, file, "x", 0o644)
	if _, err := WriteWorkspaceHomeTar(&buf, file, nil); err == nil {
		t.Error("a regular file was accepted as a workspace home")
	}
}

func TestWriteWorkspaceHomeTar_ReportsProgress(t *testing.T) {
	src := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		mustWrite(t, filepath.Join(src, n), strings.Repeat("x", 100), 0o644)
	}

	var seen []int64
	var buf bytes.Buffer
	stats, err := WriteWorkspaceHomeTar(&buf, src, func(p WorkspaceHomeTarStats) { seen = append(seen, p.Files) })
	if err != nil {
		t.Fatalf("WriteWorkspaceHomeTar: %v", err)
	}
	if len(seen) == 0 {
		t.Error("no progress was reported; a multi-GB transfer with no output is indistinguishable from a hang (D4)")
	}
	if stats.Files != 3 {
		t.Errorf("stats.Files = %d, want 3", stats.Files)
	}
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

func tarEntryNames(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mkfifoForTest creates a FIFO, the cheapest irregular file to make in a test.
// A real workspace home grows sockets rather than FIFOs (an agent's IPC
// endpoint), but the classification the writer applies is the same for both.
func mkfifoForTest(path string) error { return syscall.Mkfifo(path, 0o600) }

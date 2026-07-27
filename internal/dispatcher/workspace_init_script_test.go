package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d), the
// dispatcher half: the ONE place that resolves where a workspace's init.sh
// lives now also writes it, so the CLI can put a script somewhere the daemon
// will actually read.
//
// Every test here is about that identity. A store that wrote to a
// second, independently-derived path would pass any test that only checks
// "the bytes came back" — the failure this file exists to catch is the one
// where the write lands somewhere dispatch never looks, which is exactly what
// `podman cp` was working around before this PR.

// TestWorkspaceInitScriptStore_PathIsTheXDGConfigLocation pins the layout
// itself, so a change to it is a deliberate act rather than a silent one: the
// path is the same $XDG_CONFIG_HOME/boid/workspaces/<slug>/init.sh
// docs/{ja,en}/guide/workspace-home.md documents, resolved on the DAEMON —
// which under the container deploy means inside the daemon's own volume.
func TestWorkspaceInitScriptStore_PathIsTheXDGConfigLocation(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)

	got, err := WorkspaceInitScriptStore{}.Path("team-a")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(configDir, "boid", "workspaces", "team-a", "init.sh")
	if got != want {
		t.Errorf("Path(\"team-a\") = %q, want %q", got, want)
	}
}

// TestWorkspaceInitScriptStore_PathMatchesTheOneDispatchReads is the
// wiring-seam guard at the type level: the store must resolve through the very
// same function resolveWorkspaceHome hands to readIfExists, not a second copy
// of the layout that happens to agree today.
func TestWorkspaceInitScriptStore_PathMatchesTheOneDispatchReads(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	fromStore, err := WorkspaceInitScriptStore{}.Path("team-a")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	fromDispatch, err := workspaceInitScriptPath("team-a")
	if err != nil {
		t.Fatalf("workspaceInitScriptPath: %v", err)
	}
	if fromStore != fromDispatch {
		t.Errorf("store path %q != the path resolveWorkspaceHome reads (%q)", fromStore, fromDispatch)
	}
}

// TestWorkspaceInitScriptStore_WrittenScriptIsWhatDispatchRuns is the
// end-to-end wiring proof this PR exists for: a script written through the
// store is the script the init container actually executes, and the completion
// marker records ITS hash.
//
// It runs the real wrapper through the bash-backed init executor rather than a
// recording fake, for the reason that backend's own doc comment gives: a fake
// that only captured the request would stay green while the bytes never
// reached a shell.
func TestWorkspaceInitScriptStore_WrittenScriptIsWhatDispatchRuns(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	script := "#!/bin/sh\necho written-through-the-store > \"$BOID_WORKSPACE_HOME/marker.txt\"\n"
	if err := (WorkspaceInitScriptStore{}).Write("myws", []byte(script)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	r, be := newWorkspaceHomeTestRunnerWithBackend(t)
	volume, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(be.dirFor(volume), "marker.txt"))
	if err != nil {
		t.Fatalf("the init script written through the store never ran: %v", err)
	}
	if strings.TrimSpace(string(got)) != "written-through-the-store" {
		t.Errorf("init script side effect = %q, want %q", got, "written-through-the-store")
	}

	// And the marker hashes exactly the bytes the store persisted, so the next
	// dispatch's skip decision is about this content and not some re-encoding
	// of it.
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	marker, ok, err := readWorkspaceHomeMarker(workspaceHomeMarkerPath(metaDir, "myws"))
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	sum := sha256.Sum256([]byte(script))
	if want := hex.EncodeToString(sum[:]); marker.ScriptSHA256 != want {
		t.Errorf("marker script sha = %q, want %q (the bytes the store wrote)", marker.ScriptSHA256, want)
	}
}

// TestWorkspaceInitScriptStore_WritePermissions pins 0600 on the file and 0700
// on the directory.
//
// The script is not executable and must not become so: boid never execs the
// configured file (it reads the bytes and feeds them to `bash` inside the init
// container — see buildWorkspaceInitScript), so an executable bit would
// advertise a way of running it that does not exist. 0600 is the same posture
// every other daemon-owned file gets; the content is operator-authored shell
// that runs with the workspace's full network access.
func TestWorkspaceInitScriptStore_WritePermissions(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	if err := (WorkspaceInitScriptStore{}).Write("team-a", []byte("echo hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path, err := WorkspaceInitScriptStore{}.Path("team-a")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat init.sh: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("init.sh mode = %o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat init.sh dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("init.sh dir mode = %o, want 0700", got)
	}
}

// TestWorkspaceInitScriptStore_WriteTightensAPreExistingDirectory is PR9 codex
// round 2, Minor 4.
//
// os.MkdirAll applies its mode only to directories it CREATES, so the 0700
// above was a property of a fresh install and nothing else. The directory this
// store writes into predates the store on every machine that already has
// workspaces: on the install this plan doc migrates it is 0775, group-writable,
// created by the pre-PR9 hand-managed workflow (measured 2026-07-27).
//
// The file's own 0600 protects the CONTENT, and that is not the exposure. A
// group-writable directory lets anyone in the group unlink init.sh and put
// their own in its place — 0600 and all — and that script then runs in the
// workspace's init container with the workspace's full network access, holding
// its harness credentials. The write is where the store can say what the mode
// has to be, so it says it every time rather than only on the install where it
// happened to create the directory itself.
func TestWorkspaceInitScriptStore_WriteTightensAPreExistingDirectory(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	path, err := WorkspaceInitScriptStore{}.Path("team-a")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	// Exactly what the pre-PR9 workflow left behind.
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		t.Fatalf("stage the pre-existing directory: %v", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o775); err != nil {
		t.Fatalf("chmod the pre-existing directory: %v", err)
	}

	if err := (WorkspaceInitScriptStore{}).Write("team-a", []byte("echo hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat init.sh dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("init.sh dir mode = %o after writing into a pre-existing 0775 directory, want 0700 — "+
			"anyone in the group can still replace the script the workspace's init container runs", got)
	}
}

// TestWorkspaceInitScriptStore_WriteLeavesNoTemporaryFilesBehind pins the
// atomic-publish protocol's cleanup: an overwrite goes through a sibling temp
// file, and that sibling must not survive the rename.
//
// It matters here more than for config.yaml: the init script's directory is
// per-workspace and an operator looking at it to answer "what does this
// workspace run" should see exactly one file.
func TestWorkspaceInitScriptStore_WriteLeavesNoTemporaryFilesBehind(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	store := WorkspaceInitScriptStore{}
	if err := store.Write("team-a", []byte("v1\n")); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	if err := store.Write("team-a", []byte("v2\n")); err != nil {
		t.Fatalf("Write v2: %v", err)
	}

	path, err := store.Path("team-a")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "init.sh" {
		t.Errorf("workspace config dir contains %v, want exactly [init.sh]", names)
	}

	data, exists, err := store.Read("team-a")
	if err != nil || !exists {
		t.Fatalf("Read: exists=%v err=%v", exists, err)
	}
	if string(data) != "v2\n" {
		t.Errorf("Read = %q, want the second write %q", data, "v2\n")
	}
}

// TestWorkspaceInitScriptStore_ReadAbsent reports absence rather than an error
// — a workspace with no init.sh is the pass-through class
// (docs/plans/home-workspace-volume.md), not a fault.
func TestWorkspaceInitScriptStore_ReadAbsent(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	data, exists, err := WorkspaceInitScriptStore{}.Read("team-a")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if exists {
		t.Errorf("Read reported exists=true for a workspace with no init.sh (data=%q)", data)
	}
}

// TestWorkspaceInitScriptStore_Remove reports whether there was anything to
// remove, so the API layer can tell "cleared it" from "there was nothing".
func TestWorkspaceInitScriptStore_Remove(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	store := WorkspaceInitScriptStore{}

	removed, err := store.Remove("team-a")
	if err != nil {
		t.Fatalf("Remove (absent): %v", err)
	}
	if removed {
		t.Error("Remove reported removed=true for a workspace that had no init.sh")
	}

	if err := store.Write("team-a", []byte("echo hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	removed, err = store.Remove("team-a")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove reported removed=false after a successful Write")
	}
	if _, exists, err := store.Read("team-a"); err != nil || exists {
		t.Errorf("init.sh still present after Remove: exists=%v err=%v", exists, err)
	}
}

// TestWorkspaceInitScriptStore_RejectsInvalidSlug keeps the slug out of
// filepath.Join without a check. The API layer validates too, but this store
// takes a raw string and joins it into a path — a traversal guard belongs at
// the place that does the joining, not only at the place that happens to call
// it today.
func TestWorkspaceInitScriptStore_RejectsInvalidSlug(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	store := WorkspaceInitScriptStore{}

	for _, slug := range []string{"", "../../etc", "Team-A", "a/b"} {
		if _, err := store.Path(slug); err == nil {
			t.Errorf("Path(%q) accepted an invalid workspace slug", slug)
		}
		if _, _, err := store.Read(slug); err == nil {
			t.Errorf("Read(%q) accepted an invalid workspace slug", slug)
		}
		if err := store.Write(slug, []byte("x")); err == nil {
			t.Errorf("Write(%q) accepted an invalid workspace slug", slug)
		}
		if _, err := store.Remove(slug); err == nil {
			t.Errorf("Remove(%q) accepted an invalid workspace slug", slug)
		}
	}
}

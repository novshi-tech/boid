package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// These are logic-only unit tests that never exec a real git binary — both
// return before performCloneSteps reaches its first runGit call. The
// end-to-end sequence (real git clone/checkout against a real fixture repo)
// is covered by clone_e2e_test.go (//go:build e2e). Real git subprocess
// calls assume a host environment (writable TempDir, unrestricted git),
// which the default `go test ./...` run does not guarantee — historically
// this package's dev loop could run inside a boid sandbox whose git was
// broker-dispatched and policy-restricted (docs/plans/git-gateway-cutover.md
// PR5 note; the broker-mediated git builtin itself was retired in PR8).

func TestPerformClone_DisabledIsNoOp(t *testing.T) {
	target := filepath.Join(t.TempDir(), "workspace")
	err := performClone(sandbox.CloneSpec{Enabled: false, TargetDir: target}, nil)
	if err != nil {
		t.Fatalf("performClone with Enabled=false: %v", err)
	}
}

func TestPerformClone_MissingRequiredFieldsErrors(t *testing.T) {
	cases := []sandbox.CloneSpec{
		{Enabled: true, TargetDir: "/x", Branch: "main", BaseBranch: "main"},   // URL missing
		{Enabled: true, URL: "file:///x", Branch: "main", BaseBranch: "main"},  // TargetDir missing
		{Enabled: true, URL: "file:///x", TargetDir: "/x", BaseBranch: "main"}, // Branch missing
		{Enabled: true, URL: "file:///x", TargetDir: "/x", Branch: "main"},     // BaseBranch missing
	}
	for i, cs := range cases {
		if err := performClone(cs, nil); err == nil {
			t.Errorf("case %d: expected error for incomplete CloneSpec %+v", i, cs)
		}
	}
}

// TestClearDirContentsPreservesDirEntryButRemovesChildren pins the fix for
// a real bug found alongside PR #736 ("runner clone: remove existing target
// dir /workspace: unlinkat //workspace: device or resource busy"), which
// fired on *every* clone-enabled dispatch (not just reopen) once the daemon
// had RuntimesDir configured — the production/e2e default.
//
// TargetDir ("/workspace/<name>" in production — a name-scoped subdirectory
// of sandboxCloneTargetDir, workspace 親化リファクタリング, nose 2026-07-13
// decision) is a mount point in the real sandbox: dispatcher's cloneMounts bind-mounts it from a
// host-backed per-job runtime directory. os.RemoveAll(dir)'s final rmdir on
// an active mount point is refused by the kernel with EBUSY. clearDirContents
// must remove every entry *inside* dir while leaving dir's own directory
// entry untouched — verified here via os.SameFile, which compares the
// underlying identity (inode+device on Linux), not merely that os.Stat still
// succeeds (a remove-then-recreate would also satisfy that weaker check).
func TestClearDirContentsPreservesDirEntryButRemovesChildren(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	before, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	if err := clearDirContents(dir); err != nil {
		t.Fatalf("clearDirContents: %v", err)
	}

	after, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("clearDirContents replaced dir's own directory entry instead of only clearing its contents (would EBUSY on a real mount point)")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir after clear: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir still has entries after clearDirContents: %v", entries)
	}
}

// TestClearDirContentsMissingDirIsNoop pins the first-dispatch case: a fresh
// runtime directory whose TargetDir has never been created yet must not
// error — the very first clone attempt for a job (not just a "reopen") hits
// this path unconditionally.
func TestClearDirContentsMissingDirIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := clearDirContents(dir); err != nil {
		t.Fatalf("clearDirContents on a missing dir: %v", err)
	}
}

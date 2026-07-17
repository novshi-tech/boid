package skills_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

func TestDeployAll_CreatesAllSkills(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}

	for _, skillName := range []string{"boid-web", "boid-orchestrate", "boid-task"} {
		content, err := os.ReadFile(filepath.Join(baseDir, skillName, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s/SKILL.md: %v", skillName, err)
		}
		if !strings.Contains(string(content), skillName) {
			t.Errorf("%s/SKILL.md missing skill name", skillName)
		}
	}

	for _, ref := range []string{"data-model.md", "builtins.md", "state-machine.md"} {
		path := filepath.Join(baseDir, "boid-task", "references", ref)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("boid-task reference file missing: %s", ref)
		}
	}
}

func TestDeployAll_Idempotent(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	content1, _ := os.ReadFile(filepath.Join(baseDir, "boid-task", "SKILL.md"))

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}
	content2, _ := os.ReadFile(filepath.Join(baseDir, "boid-task", "SKILL.md"))

	if string(content1) != string(content2) {
		t.Error("idempotent deploy changed SKILL.md content")
	}
}

func TestDeployAll_UpdatesChangedFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}

	stale := filepath.Join(baseDir, "boid-task", "SKILL.md")
	if err := os.WriteFile(stale, []byte("old content"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}

	content, _ := os.ReadFile(stale)
	if string(content) == "old content" {
		t.Error("DeployAll did not update stale SKILL.md")
	}
	if !strings.Contains(string(content), "boid-task") {
		t.Error("updated SKILL.md missing expected content")
	}
}

// TestDeployAll_NoLeftoverTempFiles pins the atomic-write contract (docs/plans/
// home-workspace-volume.md Phase 4 PR3): deploySkill writes each file via a
// sibling temp file + rename rather than os.WriteFile directly, so a crash
// mid-write never leaves a partial file at the final path. This test can't
// simulate the crash itself (flaky by nature — see the plan's note), so it
// pins the observable half of the contract instead: after a normal (fresh)
// and a repeat (idempotent, some-files-changed) run, no stray temp file is
// left behind anywhere under baseDir.
func TestDeployAll_NoLeftoverTempFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	assertNoTempFiles(t, baseDir)

	// Force a real rewrite (not a no-op) on the second pass so the atomic
	// write path actually executes again, not just the bytes.Equal skip.
	stale := filepath.Join(baseDir, "boid-task", "SKILL.md")
	if err := os.WriteFile(stale, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, forces a rewrite): %v", err)
	}
	assertNoTempFiles(t, baseDir)
}

// TestDeployAll_NoOpWhenUnchanged asserts the "already up to date" fast path:
// a second DeployAll call over an already-current baseDir does not touch the
// file at all (mtime unchanged), matching the doc comment's "Files are only
// written when their content differs" contract without introducing a new
// version scheme.
func TestDeployAll_NoOpWhenUnchanged(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	target := filepath.Join(baseDir, "boid-web", "SKILL.md")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after 2nd deploy: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed on a no-op re-deploy: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

// The tests below pin the symlink-attack defense added in response to a
// codex Blocker on PR #789 (docs/plans/home-workspace-volume.md Phase 4
// PR3): baseDir is workspace HOME's `.claude/skills`, which is rw bind
// mounted into the sandbox. A compromised or malicious job can replace any
// path component under baseDir — including baseDir's own skill dirs and
// their subdirs — with a symlink to an arbitrary host path. DeployAll must
// refuse to follow such symlinks rather than silently writing through them
// as the daemon's own (uid 1000, real user permissions) process.

// TestDeployAll_RejectsPreexistingSymlinkSkillDir pins the case where a
// skill directory itself (e.g. baseDir/boid-task) was replaced with a
// symlink to an attacker-chosen directory before DeployAll ever runs (e.g.
// the very first dispatch against a workspace home whose .claude/skills
// tree was tampered with out of band). DeployAll must error out instead of
// creating files inside the symlink target.
func TestDeployAll_RejectsPreexistingSymlinkSkillDir(t *testing.T) {
	baseDir := t.TempDir()
	attackTarget := t.TempDir()

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("mkdir baseDir: %v", err)
	}
	if err := os.Symlink(attackTarget, filepath.Join(baseDir, "boid-task")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := skills.DeployAll(baseDir); err == nil {
		t.Fatal("DeployAll: expected error for symlinked skill dir, got nil")
	}

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received unexpected entries: %v", entries)
	}
}

// TestDeployAll_RejectsSymlinkSwapAfterInitialDeploy pins the case where a
// skill subdirectory (baseDir/boid-task/references) that DeployAll itself
// created on a prior run is swapped for a symlink before the next dispatch's
// DeployAll call. This is the concrete attack the review described: a
// sandboxed job replaces a nested skill dir with a symlink, hoping the next
// daemon-side sync follows it.
func TestDeployAll_RejectsSymlinkSwapAfterInitialDeploy(t *testing.T) {
	baseDir := t.TempDir()
	attackTarget := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	refsDir := filepath.Join(baseDir, "boid-task", "references")
	if err := os.RemoveAll(refsDir); err != nil {
		t.Fatalf("remove references dir: %v", err)
	}
	if err := os.Symlink(attackTarget, refsDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := skills.DeployAll(baseDir); err == nil {
		t.Fatal("DeployAll: expected error for symlinked references dir, got nil")
	}

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received unexpected entries: %v", entries)
	}
}

// TestDeployAll_RejectsSymlinkBaseDirItself pins the case flagged in the
// review as the flock-based fallback's blind spot: baseDir itself
// (`.claude/skills`) is a symlink, not just something beneath it.
func TestDeployAll_RejectsSymlinkBaseDirItself(t *testing.T) {
	parent := t.TempDir()
	attackTarget := t.TempDir()
	baseDir := filepath.Join(parent, "skills")

	if err := os.Symlink(attackTarget, baseDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := skills.DeployAll(baseDir); err == nil {
		t.Fatal("DeployAll: expected error for symlinked baseDir, got nil")
	}

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received unexpected entries: %v", entries)
	}
}

// TestDeployAll_ConcurrentSymlinkSwapNeverEscapes races a goroutine that
// repeatedly swaps a skill dir between a real directory and a symlink to an
// attack target against repeated DeployAll calls, pinning that the
// per-syscall (openat2) symlink check — rather than a separate
// Lstat/EvalSymlinks pre-check — closes the TOCTOU window a concurrent job
// could otherwise exploit. Some DeployAll calls are expected to error out
// (whenever the swap goroutine has the dir in its symlink phase); what must
// never happen is any write landing inside attackTarget.
func TestDeployAll_ConcurrentSymlinkSwapNeverEscapes(t *testing.T) {
	baseDir := t.TempDir()
	attackTarget := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	skillPath := filepath.Join(baseDir, "boid-task")
	stop := make(chan struct{})
	var swapWG sync.WaitGroup
	swapWG.Add(1)
	go func() {
		defer swapWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.RemoveAll(skillPath)
			_ = os.Symlink(attackTarget, skillPath)
			_ = os.Remove(skillPath) // removes the symlink itself, not attackTarget's contents
			_ = os.MkdirAll(skillPath, 0o755)
		}
	}()

	for i := 0; i < 100; i++ {
		_ = skills.DeployAll(baseDir) // errors expected while the dir is mid-swap; ignored here
	}
	close(stop)
	swapWG.Wait()

	entries, err := os.ReadDir(attackTarget)
	if err != nil {
		t.Fatalf("read attack target: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("attack target received %d unexpected entries after concurrent race: %v", len(entries), entries)
	}
}

// TestDeployAll_NormalRegressionAfterSafePathRewrite is a belt-and-suspenders
// regression check that the openat2-based rewrite still deploys a completely
// ordinary (never-tampered-with) tree correctly, matching
// TestDeployAll_CreatesAllSkills but run after the race test's baseDir
// pattern (fresh dir, no pre-existing state) to catch any accidental
// coupling introduced by the safe-path helpers.
func TestDeployAll_NormalRegressionAfterSafePathRewrite(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}
	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, no-op pass): %v", err)
	}

	for _, skillName := range []string{"boid-web", "boid-orchestrate", "boid-task"} {
		content, err := os.ReadFile(filepath.Join(baseDir, skillName, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s/SKILL.md: %v", skillName, err)
		}
		if !strings.Contains(string(content), skillName) {
			t.Errorf("%s/SKILL.md missing skill name", skillName)
		}
	}
}

// TestDeployAll_CleansUpStaleTempFiles pins the crash-recovery half of the
// Should-fix #1 review comment: a temp file left behind by a killed-mid-write
// daemon (no defer runs on SIGKILL) must be swept up by the *next* DeployAll
// call, not accumulate forever.
func TestDeployAll_CleansUpStaleTempFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (seed): %v", err)
	}

	stale := filepath.Join(baseDir, "boid-web", ".SKILL.md.tmp-stale-12345")
	if err := os.WriteFile(stale, []byte("leftover from a killed daemon"), 0o644); err != nil {
		t.Fatalf("write stale temp file: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd, should reclaim stale temp): %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp file was not cleaned up (stat err=%v)", err)
	}
}

// assertNoTempFiles walks dir and fails the test if any entry looks like a
// leftover atomic-write temp file (a dotfile containing "tmp" in its name).
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if !d.IsDir() && strings.HasPrefix(name, ".") && strings.Contains(name, "tmp") {
			t.Errorf("leftover temp file found: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

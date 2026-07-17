package skills_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

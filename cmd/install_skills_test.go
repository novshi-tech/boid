//go:build linux

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInstallSkills_WritesToDirFlag(t *testing.T) {
	target := filepath.Join(t.TempDir(), "skills")

	cmd := installSkillsCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	t.Cleanup(func() { cmd.SetOut(nil) }) // avoid leaking a dead buffer as this global command's writer for later tests
	if err := cmd.Flags().Set("dir", target); err != nil {
		t.Fatalf("set --dir: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("dir", "") })

	if err := runInstallSkills(cmd, nil); err != nil {
		t.Fatalf("runInstallSkills: %v", err)
	}

	for _, name := range []string{"boid-cli-workspace", "boid-cli-daemon", "boid-cli-task"} {
		if _, err := os.Stat(filepath.Join(target, name, "SKILL.md")); err != nil {
			t.Errorf("expected %s/SKILL.md to exist: %v", name, err)
		}
	}
	got := out.String()
	for _, name := range []string{"boid-cli-workspace", "boid-cli-daemon", "boid-cli-task"} {
		if !strings.Contains(got, name) {
			t.Errorf("expected output to mention %q, got %q", name, got)
		}
	}
}

func TestRunInstallSkills_IdempotentOnRepeatRun(t *testing.T) {
	target := filepath.Join(t.TempDir(), "skills")

	cmd := installSkillsCmd
	if err := cmd.Flags().Set("dir", target); err != nil {
		t.Fatalf("set --dir: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("dir", "") })
	t.Cleanup(func() { cmd.SetOut(nil) })

	var out1 bytes.Buffer
	cmd.SetOut(&out1)
	if err := runInstallSkills(cmd, nil); err != nil {
		t.Fatalf("runInstallSkills (1st): %v", err)
	}
	before, err := os.Stat(filepath.Join(target, "boid-cli-task", "SKILL.md"))
	if err != nil {
		t.Fatalf("stat after 1st run: %v", err)
	}

	var out2 bytes.Buffer
	cmd.SetOut(&out2)
	if err := runInstallSkills(cmd, nil); err != nil {
		t.Fatalf("runInstallSkills (2nd): %v", err)
	}
	after, err := os.Stat(filepath.Join(target, "boid-cli-task", "SKILL.md"))
	if err != nil {
		t.Fatalf("stat after 2nd run: %v", err)
	}
	// The actual "idempotent" claim: a repeat run with unchanged embedded
	// content must not rewrite the file (skills.DeployHostSkills' content-diff
	// skip, pinned more directly by internal/skills/deploy_host_test.go's
	// TestDeployHostSkills_Idempotent — this is the CLI-layer regression guard
	// for the same contract).
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed on a no-op re-deploy: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

func TestRunInstallSkills_RelativeDirIsResolvedToAbsolute(t *testing.T) {
	tmp := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	cmd := installSkillsCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	t.Cleanup(func() { cmd.SetOut(nil) })
	if err := cmd.Flags().Set("dir", "relative-skills-dir"); err != nil {
		t.Fatalf("set --dir: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("dir", "") })

	if err := runInstallSkills(cmd, nil); err != nil {
		t.Fatalf("runInstallSkills with a relative --dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "relative-skills-dir", "boid-cli-task", "SKILL.md")); err != nil {
		t.Errorf("expected relative --dir to resolve against cwd: %v", err)
	}
}

func TestRunInstallSkills_SymlinkedAncestorIsResolved(t *testing.T) {
	real := t.TempDir()
	parent := t.TempDir()
	symlinked := filepath.Join(parent, "claude")
	if err := os.Symlink(real, symlinked); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := filepath.Join(symlinked, "skills")

	cmd := installSkillsCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	t.Cleanup(func() { cmd.SetOut(nil) })
	if err := cmd.Flags().Set("dir", target); err != nil {
		t.Fatalf("set --dir: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("dir", "") })

	if err := runInstallSkills(cmd, nil); err != nil {
		t.Fatalf("runInstallSkills through a symlinked ancestor: %v", err)
	}
	// Files must land in the REAL directory the symlink points at, not just
	// "somewhere reachable via the symlinked path" — this is what proves the
	// symlink was resolved rather than merely tolerated by accident.
	if _, err := os.Stat(filepath.Join(real, "skills", "boid-cli-task", "SKILL.md")); err != nil {
		t.Errorf("expected files under the symlink's real target: %v", err)
	}
}

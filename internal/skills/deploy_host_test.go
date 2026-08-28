package skills_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// hostSkillNames pins the current set of host-facing skills. Kept as a
// package-level var (not inlined per-test) so every assertion below agrees
// on what "the host skill set" means.
var hostSkillNames = []string{"boid-cli-daemon", "boid-cli-task", "boid-cli-workspace"}

func TestDeployHostSkills_CreatesAllSkills(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployHostSkills(baseDir); err != nil {
		t.Fatalf("DeployHostSkills: %v", err)
	}

	for _, skillName := range hostSkillNames {
		content, err := os.ReadFile(filepath.Join(baseDir, skillName, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s/SKILL.md: %v", skillName, err)
		}
		if !strings.Contains(string(content), skillName) {
			t.Errorf("%s/SKILL.md missing skill name", skillName)
		}
	}
}

func TestDeployHostSkills_Idempotent(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployHostSkills(baseDir); err != nil {
		t.Fatalf("DeployHostSkills (1st): %v", err)
	}
	before, err := os.Stat(filepath.Join(baseDir, "boid-cli-task", "SKILL.md"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := skills.DeployHostSkills(baseDir); err != nil {
		t.Fatalf("DeployHostSkills (2nd): %v", err)
	}
	after, err := os.Stat(filepath.Join(baseDir, "boid-cli-task", "SKILL.md"))
	if err != nil {
		t.Fatalf("stat after 2nd deploy: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed on a no-op re-deploy: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

func TestHostSkillNames_MatchesDeployedSet(t *testing.T) {
	got := append([]string(nil), skills.HostSkillNames()...)
	sort.Strings(got)
	want := append([]string(nil), hostSkillNames...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("HostSkillNames() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("HostSkillNames() = %v, want %v", got, want)
		}
	}
}

// TestDeployHostSkills_DoesNotAffectSandboxSkillSet is the regression guard
// for the design invariant that host-facing skills (boid-cli-*, meant for
// ~/.claude/skills/ on the machine running the boid CLI) must never leak
// into the sandbox skill set. That set is driven entirely by
// EmbeddedSkillNames()/skillsFS (the original data/ embed), which
// build/container/Dockerfile unpacks into the runner image and
// internal/dispatcher's skillLinks symlinks into every workspace home — if
// this ever regresses to share a source with hostSkillsFS, every job
// container would start receiving host-ops skill content that documents
// commands making no sense (or being unreachable) from inside a job.
func TestDeployHostSkills_DoesNotAffectSandboxSkillSet(t *testing.T) {
	baseDir := t.TempDir()
	if err := skills.DeployHostSkills(baseDir); err != nil {
		t.Fatalf("DeployHostSkills: %v", err)
	}

	got := append([]string(nil), skills.EmbeddedSkillNames()...)
	sort.Strings(got)
	want := []string{"boid-orchestrate", "boid-signal", "boid-task", "boid-web"}

	if len(got) != len(want) {
		t.Fatalf("EmbeddedSkillNames() = %v, want %v (host skills must not appear here)", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("EmbeddedSkillNames() = %v, want %v (host skills must not appear here)", got, want)
		}
	}
}

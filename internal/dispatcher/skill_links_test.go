package dispatcher

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/integrationpack"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file pins the image-baked skill delivery that replaced the per-skill
// bind mounts: embedded skills are baked into the runner image at
// embeddedSkillsImageDir and reach every harness by symlink, exactly the way
// Integration Pack skills already did.
//
// The three properties worth pinning, because each one is what a different
// harness depends on:
//
//   - embedded skills are LINKS now, not bind mounts, so skillLinks has to
//     return them (TestSkillLinks_*);
//   - the links land in BOTH discovery roots, because no single root is read
//     by all three harnesses boid dispatches (TestBuildWorkspaceInitRequest_
//     SkillSymlinksLandInBothDiscoveryRoots);
//   - nothing declares a per-skill bind any more, which is the whole point —
//     the host-visible materialization directory those binds needed is gone
//     (TestHomeMounts_DeclaresNoPerSkillBinds).

func TestSkillLinks_IncludesEmbeddedSkillsPointingAtTheImageDir(t *testing.T) {
	embedded := skills.EmbeddedSkillNames()
	if len(embedded) == 0 {
		t.Skip("no embedded skills compiled into this binary")
	}

	byName := make(map[string]string)
	for _, l := range skillLinks("", nil) {
		byName[l.Name] = l.Target
	}
	for _, name := range embedded {
		target, ok := byName[name]
		if !ok {
			t.Errorf("skillLinks() has no entry for embedded skill %q", name)
			continue
		}
		want := filepath.Join(embeddedSkillsImageDir(), name)
		if target != want {
			t.Errorf("embedded skill %q Target = %q, want %q", name, target, want)
		}
	}
	if len(byName) != len(embedded) {
		t.Errorf("skillLinks returned %d links, want exactly the %d embedded skills", len(byName), len(embedded))
	}
}

// TestSkillLinks_EmbeddedWinsOverCollidingPackSkill carries the collision
// guard over from packSkillLinks. It matters MORE now than it did against
// bind mounts: both sides are symlinks into the same two directories, so
// without the guard the loser is decided by iteration order rather than by a
// rule.
func TestSkillLinks_EmbeddedWinsOverCollidingPackSkill(t *testing.T) {
	embedded := skills.EmbeddedSkillNames()
	if len(embedded) == 0 {
		t.Skip("no embedded skills compiled into this binary to collide with")
	}
	collidingName := embedded[0]

	links := skillLinks("", []*integrationpack.Pack{packWithSkill(collidingName, "skills/x")})
	var seen int
	for _, l := range links {
		if l.Name != collidingName {
			continue
		}
		seen++
		if want := filepath.Join(embeddedSkillsImageDir(), collidingName); l.Target != want {
			t.Errorf("colliding name %q resolved to %q, want the embedded skill at %q", collidingName, l.Target, want)
		}
	}
	if seen != 1 {
		t.Errorf("name %q appears %d times in skillLinks; a name must resolve to exactly one target", collidingName, seen)
	}
}

// TestWorkspaceHomeSkeletonDirs_IsTheDiscoveryRootsOnly pins the shrink that
// image-baking buys. The set used to carry one entry per embedded skill, so
// every release that added a skill changed it and re-ran init.sh for every
// workspace on the installation (workspaceHomeMarker.SkeletonDirs). The
// leaves are symlinks now, not bind targets, so the set is a constant of the
// discovery roots and no longer moves when the skill set does.
func TestWorkspaceHomeSkeletonDirs_IsTheDiscoveryRootsOnly(t *testing.T) {
	got := workspaceHomeSkeletonDirs()

	var want []string
	for _, root := range skillDiscoveryRoots {
		want = append(want, filepath.Dir(root), root)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("skeleton = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("skeleton = %v, want %v", got, want)
		}
	}

	for _, name := range skills.EmbeddedSkillNames() {
		for _, root := range skillDiscoveryRoots {
			leaf := filepath.Join(root, name)
			for _, dir := range got {
				if dir == leaf {
					t.Errorf("skeleton still carries the per-skill leaf %q; leaves are symlinks now, and mkdir -p'ing one would make ln -sfn nest inside it instead of replacing it", leaf)
				}
			}
		}
	}
	for _, dir := range got {
		if filepath.IsAbs(dir) {
			t.Errorf("skeleton entry %q is absolute; entries are relative to the workspace home so the prep container never needs the host path", dir)
		}
	}
}

// TestBuildWorkspaceInitRequest_SkillSymlinksLandInBothDiscoveryRoots is the
// load-bearing one. Claude Code reads only ~/.claude/skills; codex reads only
// ~/.agents/skills (plus paths outside $HOME); opencode reads both. One root
// therefore cannot serve the three harnesses boid dispatches, so the prelude
// writes every link twice.
func TestBuildWorkspaceInitRequest_SkillSymlinksLandInBothDiscoveryRoots(t *testing.T) {
	homeDir := t.TempDir()
	imageDir := t.TempDir()
	skillDir := filepath.Join(imageDir, "boid-task")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   []SkillLink{{Name: "boid-task", Target: skillDir}},
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != 0 {
		t.Fatalf("wrapper exited %d, want 0\n%s", code, out)
	}

	if len(skillDiscoveryRoots) < 2 {
		t.Fatalf("skillDiscoveryRoots = %v; this test exists because there is more than one", skillDiscoveryRoots)
	}
	for _, root := range skillDiscoveryRoots {
		linkPath := filepath.Join(homeDir, root, "boid-task")
		info, lerr := os.Lstat(linkPath)
		if lerr != nil {
			t.Errorf("stat %s: %v", filepath.Join(root, "boid-task"), lerr)
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink (mode %s)", filepath.Join(root, "boid-task"), info.Mode())
			continue
		}
		target, rerr := os.Readlink(linkPath)
		if rerr != nil {
			t.Errorf("readlink %s: %v", linkPath, rerr)
			continue
		}
		if target != skillDir {
			t.Errorf("%s -> %q, want %q", filepath.Join(root, "boid-task"), target, skillDir)
		}
		data, derr := os.ReadFile(filepath.Join(linkPath, "SKILL.md"))
		if derr != nil {
			t.Errorf("read through %s: %v", filepath.Join(root, "boid-task"), derr)
			continue
		}
		if string(data) != "hello" {
			t.Errorf("content through %s = %q, want %q", filepath.Join(root, "boid-task"), data, "hello")
		}
	}
}

// TestHomeMounts_DeclaresNoPerSkillBinds pins the removal itself. A surviving
// per-skill bind would not merely be redundant: its source is a host-visible
// path that nothing materializes any more, and homeMounts deliberately gives
// these mounts no Guard, so a vanished source aborts the job.
func TestHomeMounts_DeclaresNoPerSkillBinds(t *testing.T) {
	const homeDir = "/home/boid"
	mounts := homeMounts(homeDir, "boid-ws-home-testinst-myws")

	if len(mounts) != 1 {
		t.Fatalf("homeMounts returned %d mounts, want exactly 1 (the workspace home volume)", len(mounts))
	}
	if mounts[0].Target != homeDir {
		t.Errorf("mount target = %q, want %q", mounts[0].Target, homeDir)
	}
	if mounts[0].Type != sandbox.MountBind {
		t.Errorf("mount type = %v, want a bind of the home volume", mounts[0].Type)
	}
	for _, name := range skills.EmbeddedSkillNames() {
		for _, root := range skillDiscoveryRoots {
			leaf := filepath.Join(homeDir, root, name)
			for _, m := range mounts {
				if m.Target == leaf {
					t.Errorf("homeMounts still binds %q; embedded skills reach the home by symlink from the image now", leaf)
				}
			}
		}
	}
}

// TestSkillDiscoveryRoots_AreTheHarnessDirectoriesByName writes the two roots
// down as literals, because every other assertion about them derives its
// expectation from the same variable and so cannot notice a change to it.
//
// The values are not arbitrary and cannot be normalized away: each one is the
// directory a specific harness scans, established by reading Claude Code's
// binary (no ".agents" string in it at all, v2.1.250) and codex's and
// opencode's published docs. `.agents/skills` in particular is what makes
// codex discover skills at all — nothing else in this package would fail if it
// silently became `.bogus/skills`.
func TestSkillDiscoveryRoots_AreTheHarnessDirectoriesByName(t *testing.T) {
	want := []string{
		filepath.Join(".claude", "skills"), // Claude Code, opencode
		filepath.Join(".agents", "skills"), // codex, opencode (cross-vendor)
	}
	if len(skillDiscoveryRoots) != len(want) {
		t.Fatalf("skillDiscoveryRoots = %v, want %v", skillDiscoveryRoots, want)
	}
	for i := range want {
		if skillDiscoveryRoots[i] != want[i] {
			t.Errorf("skillDiscoveryRoots[%d] = %q, want %q — this is the directory a harness scans, not an internal name",
				i, skillDiscoveryRoots[i], want[i])
		}
	}
}

// TestEmbeddedSkillsImageDir_IsWhereTheDockerfileCopiesThem pins the one
// literal Go cannot check for itself: the constant naming a path inside the
// runner image has to be the path the Dockerfile actually unpacks the skills
// to.
//
// Without this, a drift on either side leaves every unit test green — both
// sides of every assertion in this file derive from the constant, so they move
// together — while production gets a home full of dangling symlinks. The
// prelude's `[ -d ]` check turns that into a loud failure rather than a silent
// one, but a loud failure on every dispatch of every workspace is still an
// outage; catching it here costs a string compare.
//
// Same trick, for the same class of failure, as
// TestBoidRunnerProtocolLabel_IsBakedIntoTheImage. Like that one it cannot pin
// that a BUILT image carries the directory — only that the two sources of the
// path agree.
func TestEmbeddedSkillsImageDir_IsWhereTheDockerfileCopiesThem(t *testing.T) {
	path := filepath.Join("..", "..", "build", "container", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	want := "COPY internal/skills/data " + defaultEmbeddedSkillsImageDir
	if !strings.Contains(string(data), want) {
		t.Errorf("build/container/Dockerfile does not contain %q\n"+
			"Every embedded skill is symlinked into every workspace home at that exact path; if the image unpacks them somewhere else, the init container refuses to link them and every dispatch of every workspace fails.", want)
	}
}

// TestBuildWorkspaceInitRequest_MissingSkillTargetFailsLoud is the guard for
// the failure the image-baked route made possible and the bind-mount route
// could not have: a runner image whose /opt/boid/skills is absent or stale.
//
// `ln -s` succeeds against a target that does not exist, so without the
// prelude's `[ -d ]` check the init exits 0, resolveWorkspaceHome writes a
// completion marker over a home full of dangling links, and nothing re-runs —
// the script hash, the generation, the home identity, the skeleton and the link
// set all still match, so the home stays broken even after the image is fixed.
// That is why this is a hard failure rather than a skipped link.
func TestBuildWorkspaceInitRequest_MissingSkillTargetFailsLoud(t *testing.T) {
	homeDir := t.TempDir()

	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks: []SkillLink{{
			Name:   "boid-task",
			Target: filepath.Join(t.TempDir(), "not-unpacked-in-this-image"),
		}},
		HomeID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != workspaceInitPreludeExitCode {
		t.Fatalf("wrapper exited %d, want %d — a missing skill target must fail the init, not leave a dangling symlink behind a completion marker\n%s",
			code, workspaceInitPreludeExitCode, out)
	}
	if stage := workspaceInitStageOf(out, code); stage != workspaceInitStagePrelude {
		t.Errorf("stage = %q, want %q", stage, workspaceInitStagePrelude)
	}
	for _, root := range skillDiscoveryRoots {
		if _, lerr := os.Lstat(filepath.Join(homeDir, root, "boid-task")); lerr == nil {
			t.Errorf("%s was created even though its target is missing", filepath.Join(root, "boid-task"))
		}
	}
}

// TestBuildWorkspaceInitRequest_SymlinkedSkeletonPathIsRefused pins the
// containment check on the paths the prelude is about to traverse.
//
// The workspace home is mounted read-write into every job and persists across
// them, so a job can replace `.claude` with a symlink pointing outside the
// home; `mkdir -p` follows it and the per-skill `rm -rf` then deletes
// <elsewhere>/skills/<name> recursively. The escape is bounded (the init
// container mounts only the home) but its REACH grew with image-baked skills:
// a workspace with no Integration Packs used to emit zero `rm -rf` and now
// emits one per embedded skill per root.
func TestBuildWorkspaceInitRequest_SymlinkedSkeletonPathIsRefused(t *testing.T) {
	homeDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "skills", "boid-task")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep-me"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(homeDir, ".claude")); err != nil {
		t.Fatal(err)
	}

	imageDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(imageDir, "boid-task"), 0o755); err != nil {
		t.Fatal(err)
	}
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   []SkillLink{{Name: "boid-task", Target: filepath.Join(imageDir, "boid-task")}},
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != workspaceInitPreludeExitCode {
		t.Fatalf("wrapper exited %d, want %d — a symlinked skeleton path must be refused, not traversed\n%s",
			code, workspaceInitPreludeExitCode, out)
	}
	if _, serr := os.Stat(filepath.Join(victim, "keep-me")); serr != nil {
		t.Errorf("the prelude deleted content outside the workspace home through the symlink: %v", serr)
	}
}

// TestBuildWorkspaceInitRequest_SymlinkStepFailureIsLoud pins that the stage
// checks around the symlink step are actually load-bearing.
//
// The wrapper has no `set -e`, so without them a failing `rm -rf` or `ln -sfn`
// is skipped over and the init container exits 0 — the marker gets written and
// the job starts with an undiscoverable skill and nothing in any log. The
// failure is reachable rather than theoretical: an engine-auto-created uid-0
// discovery root (the 論点 b-2 condition this codebase measures) makes both
// commands fail with EACCES, and that is most likely during the very cutover
// that converts old bind-target directories.
func TestBuildWorkspaceInitRequest_SymlinkStepFailureIsLoud(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	homeDir := t.TempDir()
	root := skillDiscoveryRoots[0]
	if err := os.MkdirAll(filepath.Join(homeDir, root), 0o755); err != nil {
		t.Fatal(err)
	}
	// Read+execute but not write: mkdir -p succeeds (the directory is already
	// there) and the ln -sfn into it does not.
	if err := os.Chmod(filepath.Join(homeDir, root), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(homeDir, root), 0o755) })

	imageDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(imageDir, "boid-task"), 0o755); err != nil {
		t.Fatal(err)
	}
	req, err := buildWorkspaceInitRequest(workspaceInitParams{
		Slug:         "myws",
		HomeSource:   "boid-ws-home-testinst-myws",
		HomeTarget:   homeDir,
		SkeletonDirs: workspaceHomeSkeletonDirs(),
		SkillLinks:   []SkillLink{{Name: "boid-task", Target: filepath.Join(imageDir, "boid-task")}},
		HomeID:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("buildWorkspaceInitRequest: %v", err)
	}

	code, out := runWorkspaceInitWrapper(t, req)
	if code != workspaceInitPreludeExitCode {
		t.Fatalf("wrapper exited %d, want %d — a symlink step that cannot write must fail the init rather than report success\n%s",
			code, workspaceInitPreludeExitCode, out)
	}
	if stage := workspaceInitStageOf(out, code); stage != workspaceInitStagePrelude {
		t.Errorf("stage = %q, want %q", stage, workspaceInitStagePrelude)
	}
}

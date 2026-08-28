package dispatcher

import (
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/integrationpack"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file pins skillLinks' guards on the PACK half of its input, both found
// missing across two Opus pre-merge review rounds, and both confirmed against
// a real shell before being fixed (not just reasoned about):
//
//   - an unsafe skill Name, gated by integrationpack.ValidSkillName's
//     allowlist (own tests in internal/integrationpack/manifest_test.go).
//     Three DENYLIST attempts here each missed something before the
//     allowlist replaced them: "/" passed a naive
//     "Name != filepath.Base(Name)" filter (filepath.Base("/") == "/"),
//     "." passed the filepath.IsLocal follow-up (filepath.IsLocal(".") is
//     true), and a NUL byte passed both (bash strips NUL from a script fed
//     on stdin, so "rm -rf -- '.claude/skills/<NUL>'" executes as "rm -rf
//     -- '.claude/skills/'"). Every one of these wipes a skill discovery
//     root wholesale.
//   - a skill Name colliding with an EMBEDDED skill's name.
//
// The embedded half is pinned in skill_links_test.go, which is also where the
// collision rule's OUTCOME (embedded wins) is asserted; what is checked here
// is only that a colliding Pack entry does not survive the filter.

func packWithSkill(name, path string) *integrationpack.Pack {
	return &integrationpack.Pack{
		Name: "test-pack", Version: "1.0.0", Dir: "/opt/boid/integrations/test-pack/1.0.0",
		Manifest: integrationpack.Manifest{
			Skills: []integrationpack.Skill{{Name: name, Path: path}},
		},
	}
}

// packLinksOnly drops the embedded entries skillLinks always prepends, so a
// case about Pack filtering can assert on counts without restating the
// binary's own skill set.
func packLinksOnly(t *testing.T, links []SkillLink) []SkillLink {
	t.Helper()
	embedded := make(map[string]bool)
	for _, name := range skills.EmbeddedSkillNames() {
		embedded[name] = true
	}
	var out []SkillLink
	for _, l := range links {
		if !embedded[l.Name] {
			out = append(out, l)
		}
	}
	return out
}

func TestSkillLinks_RejectsUnsafePackSkillNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip bool
	}{
		{name: "", skip: true},
		{name: ".", skip: true},
		{name: "..", skip: true},
		{name: "/", skip: true},
		{name: "//", skip: true},
		{name: "a/b", skip: true},
		{name: "/a", skip: true},
		{name: "../escape", skip: true},
		{name: "./x", skip: true},
		// The NUL-byte case: see this file's top-of-file comment for why
		// this specific input mattered enough to end the denylist approach.
		{name: "\x00", skip: true},
		{name: "a\x00b", skip: true},
		{name: "jira-api", skip: false},
		{name: "bitbucket-api", skip: false},
		// Not a path-shaped attack, just unusual — must still be allowed
		// through as a normal single-component name.
		{name: "a...b", skip: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links := packLinksOnly(t, skillLinks([]*integrationpack.Pack{packWithSkill(tc.name, "skills/x")}))
			found := len(links) == 1
			if tc.skip && found {
				t.Errorf("skillLinks(name=%q) kept %+v, want the skill rejected (unsafe Name)", tc.name, links)
			}
			if !tc.skip && !found {
				t.Errorf("skillLinks(name=%q) kept %+v, want exactly one link (a normal name must not be rejected)", tc.name, links)
			}
		})
	}
}

// TestSkillLinks_RejectsPackNameCollidingWithEmbeddedSkill is B2's regression
// guard: a Pack cannot claim an embedded skill's name and take over its
// discovery-root entry. TestSkillLinks_EmbeddedWinsOverCollidingPackSkill
// (skill_links_test.go) asserts the other side of it — that the name still
// resolves, to the embedded skill.
func TestSkillLinks_RejectsPackNameCollidingWithEmbeddedSkill(t *testing.T) {
	embedded := skills.EmbeddedSkillNames()
	if len(embedded) == 0 {
		t.Skip("no embedded skills compiled into this binary to collide with")
	}
	collidingName := embedded[0]

	pack := packWithSkill(collidingName, "skills/x")
	for _, l := range skillLinks([]*integrationpack.Pack{pack}) {
		if l.Name == collidingName && l.Target == filepath.Join(pack.Dir, "skills", "x") {
			t.Fatalf("skillLinks let a Pack skill claim embedded skill name %q and point it into the Pack: %+v", collidingName, l)
		}
	}
}

// TestSkillLinks_PackTargetIsPackDirJoinPath pins the (unvalidated-by-this-
// function, but load-bearing) Target computation for a Pack skill: pack.Dir +
// skill.Path, exactly what the init container already has baked into its own
// image — never a bind-mount source.
func TestSkillLinks_PackTargetIsPackDirJoinPath(t *testing.T) {
	pack := packWithSkill("jira-api", filepath.Join("skills", "jira-api"))
	links := packLinksOnly(t, skillLinks([]*integrationpack.Pack{pack}))
	if len(links) != 1 {
		t.Fatalf("skillLinks kept %+v, want exactly 1 Pack link", links)
	}
	want := filepath.Join(pack.Dir, "skills", "jira-api")
	if links[0].Target != want {
		t.Errorf("Target = %q, want %q", links[0].Target, want)
	}
}

// TestSkillLinkMarkerStrings_IsOrderInsensitiveViaEqualStringSets pins the
// marker comparison's actual behavior: two skillLinks results differing only
// in order must compare equal so a Pack registry whose enumeration order
// changed (LoadPacks walks a directory) does not force a spurious re-init.
func TestSkillLinkMarkerStrings_IsOrderInsensitiveViaEqualStringSets(t *testing.T) {
	a := []SkillLink{{Name: "jira-api", Target: "/opt/x"}, {Name: "slack-api", Target: "/opt/y"}}
	b := []SkillLink{{Name: "slack-api", Target: "/opt/y"}, {Name: "jira-api", Target: "/opt/x"}}
	if !equalStringSets(skillLinkMarkerStrings(a), skillLinkMarkerStrings(b)) {
		t.Errorf("skillLinkMarkerStrings sets differ by order alone: %v vs %v",
			skillLinkMarkerStrings(a), skillLinkMarkerStrings(b))
	}
}

// TestSkillLinkMarkerStrings_DetectsTargetChange pins the other half: a Pack
// upgraded to a new version directory changes Target (the version segment)
// while Name stays put, and that must register as a change — an upgraded
// Pack's symlink must not be left pointing at a version directory the current
// image no longer has.
func TestSkillLinkMarkerStrings_DetectsTargetChange(t *testing.T) {
	before := []SkillLink{{Name: "jira-api", Target: "/opt/boid/integrations/jira-cloud/1.0.0/skills/jira-api"}}
	after := []SkillLink{{Name: "jira-api", Target: "/opt/boid/integrations/jira-cloud/1.1.0/skills/jira-api"}}
	if equalStringSets(skillLinkMarkerStrings(before), skillLinkMarkerStrings(after)) {
		t.Error("a Pack version bump (Target changed, Name unchanged) did not register as a change")
	}
}

// TestSkillLinkMarkerStrings_DetectsAnAddedEmbeddedSkill pins what this
// comparison inherited from workspaceHomeMarker.SkeletonDirs when the
// embedded set moved off bind mounts. The skeleton used to carry one entry
// per embedded skill, so a release that shipped a new one changed it and
// re-ran init.sh; the skeleton is a constant now, and this set is the only
// thing left that notices.
func TestSkillLinkMarkerStrings_DetectsAnAddedEmbeddedSkill(t *testing.T) {
	before := skillLinks(nil)
	after := append(append([]SkillLink(nil), before...),
		SkillLink{Name: "boid-newthing", Target: filepath.Join(embeddedSkillsImageDir, "boid-newthing")})
	if equalStringSets(skillLinkMarkerStrings(before), skillLinkMarkerStrings(after)) {
		t.Error("a release that added an embedded skill did not register as a change; the new skill would stay undiscoverable in every already-initialized workspace home")
	}
}

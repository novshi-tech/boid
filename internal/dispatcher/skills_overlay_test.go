package dispatcher

import (
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/integrationpack"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file pins packSkillLinks' two guards, both found missing across two
// Opus pre-merge review rounds, and both confirmed against a real shell
// before being fixed (not just reasoned about):
//
//   - an unsafe skill Name, gated by integrationpack.ValidSkillName's
//     allowlist (own tests in internal/integrationpack/manifest_test.go).
//     Three DENYLIST attempts here each missed something before the
//     allowlist replaced them: "/" passed a naive
//     "Name != filepath.Base(Name)" filter (filepath.Base("/") == "/"),
//     "." passed the filepath.IsLocal follow-up (filepath.IsLocal(".") is
//     true), and a NUL byte passed both (bash strips NUL from a script fed
//     on stdin, so "rm -rf -- '.claude/skills/<NUL>'" executes as "rm -rf
//     -- '.claude/skills/'"). Every one of these wipes the directory every
//     embedded skill's bind target lives under, replacing it with a
//     symlink into the Pack.
//   - a skill Name colliding with an EMBEDDED skill's name: nothing
//     guarded against it, and the same rm-then-symlink pair would replace
//     that embedded skill's bind-mounted directory with a symlink into the
//     Pack.

func packWithSkill(name, path string) *integrationpack.Pack {
	return &integrationpack.Pack{
		Name: "test-pack", Version: "1.0.0", Dir: "/opt/boid/integrations/test-pack/1.0.0",
		Manifest: integrationpack.Manifest{
			Skills: []integrationpack.Skill{{Name: name, Path: path}},
		},
	}
}

func TestPackSkillLinks_RejectsUnsafeNames(t *testing.T) {
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
			links := packSkillLinks([]*integrationpack.Pack{packWithSkill(tc.name, "skills/x")})
			found := len(links) == 1
			if tc.skip && found {
				t.Errorf("packSkillLinks(name=%q) = %+v, want the skill rejected (unsafe Name)", tc.name, links)
			}
			if !tc.skip && !found {
				t.Errorf("packSkillLinks(name=%q) = %+v, want exactly one link (a normal name must not be rejected)", tc.name, links)
			}
		})
	}
}

// TestPackSkillLinks_RejectsNameCollidingWithEmbeddedSkill is B2's
// regression guard: a Pack cannot claim an embedded skill's name and take
// over its bind-mounted .claude/skills/<name> directory via symlink.
func TestPackSkillLinks_RejectsNameCollidingWithEmbeddedSkill(t *testing.T) {
	embedded := skills.EmbeddedSkillNames()
	if len(embedded) == 0 {
		t.Skip("no embedded skills compiled into this binary to collide with")
	}
	collidingName := embedded[0]

	links := packSkillLinks([]*integrationpack.Pack{packWithSkill(collidingName, "skills/x")})
	for _, l := range links {
		if l.Name == collidingName {
			t.Fatalf("packSkillLinks let a Pack skill claim embedded skill name %q: %+v", collidingName, links)
		}
	}
}

// TestPackSkillLinks_TargetIsPackDirJoinPath pins the (unvalidated-by-this-
// function, but load-bearing) Target computation: pack.Dir + skill.Path,
// exactly what the init container already has baked into its own image —
// never a bind-mount source.
func TestPackSkillLinks_TargetIsPackDirJoinPath(t *testing.T) {
	pack := packWithSkill("jira-api", filepath.Join("skills", "jira-api"))
	links := packSkillLinks([]*integrationpack.Pack{pack})
	if len(links) != 1 {
		t.Fatalf("packSkillLinks = %+v, want exactly 1 link", links)
	}
	want := filepath.Join(pack.Dir, "skills", "jira-api")
	if links[0].Target != want {
		t.Errorf("Target = %q, want %q", links[0].Target, want)
	}
}

// TestPackSkillLinkMarkerStrings_IsOrderInsensitiveViaEqualStringSets pins
// the marker comparison's actual behavior: two packSkillLinks results
// differing only in order must compare equal so a Pack registry whose
// enumeration order changed (LoadPacks walks a directory) does not force a
// spurious re-init.
func TestPackSkillLinkMarkerStrings_IsOrderInsensitiveViaEqualStringSets(t *testing.T) {
	a := []PackSkillLink{{Name: "jira-api", Target: "/opt/x"}, {Name: "slack-api", Target: "/opt/y"}}
	b := []PackSkillLink{{Name: "slack-api", Target: "/opt/y"}, {Name: "jira-api", Target: "/opt/x"}}
	if !equalStringSets(packSkillLinkMarkerStrings(a), packSkillLinkMarkerStrings(b)) {
		t.Errorf("packSkillLinkMarkerStrings sets differ by order alone: %v vs %v",
			packSkillLinkMarkerStrings(a), packSkillLinkMarkerStrings(b))
	}
}

// TestPackSkillLinkMarkerStrings_DetectsTargetChange pins the other half: a
// Pack upgraded to a new version directory changes Target (the version
// segment) while Name stays put, and that must register as a change — an
// upgraded Pack's symlink must not be left pointing at a version directory
// the current image no longer has.
func TestPackSkillLinkMarkerStrings_DetectsTargetChange(t *testing.T) {
	before := []PackSkillLink{{Name: "jira-api", Target: "/opt/boid/integrations/jira-cloud/1.0.0/skills/jira-api"}}
	after := []PackSkillLink{{Name: "jira-api", Target: "/opt/boid/integrations/jira-cloud/1.1.0/skills/jira-api"}}
	if equalStringSets(packSkillLinkMarkerStrings(before), packSkillLinkMarkerStrings(after)) {
		t.Error("a Pack version bump (Target changed, Name unchanged) did not register as a change")
	}
}

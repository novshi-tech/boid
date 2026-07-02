package claude

import (
	"testing"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/skills"
)

// findBySource returns the first binding whose Source matches, or false.
func findBySource(mounts []adapters.BindMount, src string) (adapters.BindMount, bool) {
	for _, m := range mounts {
		if m.Source == src {
			return m, true
		}
	}
	return adapters.BindMount{}, false
}

// TestBindings_BaseMounts pins the four CLI-state mounts claude.Run() needs
// (absorbed from the retired boid-kits claude-code kit in Phase 3-e). A drift
// here — a mode flipped ro↔rw, IsFile dropped from ~/.claude.json, or Optional
// removed — would either break claude inside the sandbox or fail dispatch on a
// host that lacks the source, so each field is asserted explicitly.
func TestBindings_BaseMounts(t *testing.T) {
	home := "/home/test"
	mounts := New().Bindings(home)

	cases := []struct {
		src      string
		wantMode string
		wantFile bool
	}{
		{home + "/.local/bin", "", false},          // ro dir → also joins PATH
		{home + "/.local/share/claude", "", false}, // ro shared data
		{home + "/.claude", "rw", false},           // rw config/state
		{home + "/.claude.json", "rw", true},       // rw main settings (file bind)
	}
	for _, c := range cases {
		m, ok := findBySource(mounts, c.src)
		if !ok {
			t.Errorf("missing base mount Source=%q in %v", c.src, mounts)
			continue
		}
		if m.Mode != c.wantMode {
			t.Errorf("%s: Mode=%q, want %q", c.src, m.Mode, c.wantMode)
		}
		if m.IsFile != c.wantFile {
			t.Errorf("%s: IsFile=%v, want %v", c.src, m.IsFile, c.wantFile)
		}
		if !m.Optional {
			t.Errorf("%s: Optional=false, want true (missing host source must be skipped, not fail dispatch)", c.src)
		}
		if m.Target != "" {
			t.Errorf("%s: Target=%q, want \"\" (same-path recipe; dispatcher skips Source==Target)", c.src, m.Target)
		}
	}
}

// TestBindings_SurfacesEachEmbeddedSkillAtClaudeSkillsPath asserts every
// embedded skill is exposed at ~/.claude/skills/<name> so /boid-task /
// /boid-orchestrate / /boid-web resolve inside claude. Unlike the base mounts
// these need an explicit distinct Target (host path lives under
// ~/.local/share/boid/skills).
func TestBindings_SurfacesEachEmbeddedSkillAtClaudeSkillsPath(t *testing.T) {
	home := "/home/test"
	mounts := New().Bindings(home)

	names := skills.EmbeddedSkillNames()
	if len(names) == 0 {
		t.Fatal("EmbeddedSkillNames returned empty; nothing to test")
	}
	for _, name := range names {
		wantSrc := home + "/.local/share/boid/skills/" + name
		wantTgt := home + "/.claude/skills/" + name
		m, ok := findBySource(mounts, wantSrc)
		if !ok {
			t.Errorf("skill %q: no binding with Source=%q", name, wantSrc)
			continue
		}
		if m.Target != wantTgt {
			t.Errorf("skill %q: Target=%q, want %q", name, m.Target, wantTgt)
		}
		if !m.Optional {
			t.Errorf("skill %q: Optional=false, want true", name)
		}
		if m.Mode != "" {
			t.Errorf("skill %q: Mode=%q, want \"\" (skills are read-only)", name, m.Mode)
		}
	}
}

// TestBindings_NoTargetCollisions guards path uniqueness across the full set:
// the sandbox refuses to mount two binds onto the same target, so a base mount
// and a skill mount must never resolve to the same effective path.
func TestBindings_NoTargetCollisions(t *testing.T) {
	mounts := New().Bindings("/home/test")

	seen := map[string]string{}
	for _, m := range mounts {
		key := m.Target
		if key == "" {
			key = m.Source
		}
		if prev, ok := seen[key]; ok {
			t.Errorf("duplicate mount target %q (sources: %q and %q)", key, prev, m.Source)
		}
		seen[key] = m.Source
	}
}

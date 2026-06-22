package opencode

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// Override resolveCommand for deterministic test runs: this test should pass
// regardless of whether the host has opencode installed.
func withMissingOpencode(t *testing.T) {
	t.Helper()
	saved := resolveCommand
	resolveCommand = func(string) (string, error) {
		return "", errors.New("test stub: opencode not installed")
	}
	t.Cleanup(func() { resolveCommand = saved })
}

// Mirror of codex/bindings_test.go: each embedded skill must surface inside
// the sandbox at ~/.claude/skills/<name> — the path opencode auto-detects
// (same convention as claude). The bootstrap prompt references this same
// canonical path regardless of which harness is running.
func TestBindings_SurfacesEachEmbeddedSkillAtClaudeSkillsPath(t *testing.T) {
	withMissingOpencode(t)
	home := "/home/test"
	a := New()
	mounts := a.Bindings(home)

	names := skills.EmbeddedSkillNames()
	if len(names) == 0 {
		t.Fatal("EmbeddedSkillNames returned empty; nothing to test")
	}

	for _, name := range names {
		wantSrc := home + "/.local/share/boid/skills/" + name
		wantTgt := home + "/.claude/skills/" + name
		found := false
		for _, m := range mounts {
			if m.Source == wantSrc {
				found = true
				if m.Target != wantTgt {
					t.Errorf("skill %q: Target=%q, want %q", name, m.Target, wantTgt)
				}
				if !m.Optional {
					t.Errorf("skill %q: Optional=false, must be true so a missing host skill dir is skipped", name)
				}
				if m.Mode != "" {
					t.Errorf("skill %q: Mode=%q, want \"\" (ro) — skills are read-only", name, m.Mode)
				}
				break
			}
		}
		if !found {
			t.Errorf("skill %q: no binding with Source=%q in %v", name, wantSrc, mounts)
		}
	}
}

func TestBindings_NoTargetCollisions(t *testing.T) {
	withMissingOpencode(t)
	home := "/home/test"
	mounts := New().Bindings(home)

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

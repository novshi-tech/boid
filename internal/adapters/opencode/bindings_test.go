package opencode

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

type fakeDirEntry struct {
	name  string
	isDir bool
}

func (f fakeDirEntry) Name() string               { return f.name }
func (f fakeDirEntry) IsDir() bool                { return f.isDir }
func (f fakeDirEntry) Type() fs.FileMode          { return 0 }
func (f fakeDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

// withFakeSkillsDir overrides readSkillsDir for deterministic test runs.
func withFakeSkillsDir(t *testing.T, entries []fs.DirEntry, err error) {
	t.Helper()
	saved := readSkillsDir
	readSkillsDir = func(string) ([]fs.DirEntry, error) {
		return entries, err
	}
	t.Cleanup(func() { readSkillsDir = saved })
}

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

// opencode persists its selected model and UI settings under the XDG state
// dir (~/.local/state/opencode/model.json etc.). This tree must be bound rw so
// the model the user picked on the host applies inside the sandbox instead of
// silently falling back to opencode's built-in default. Regression guard for
// the "wrong default model inside boid agent opencode" bug.
func TestBindings_BindsStateTreeReadWrite(t *testing.T) {
	withMissingOpencode(t)
	home := "/home/test"
	mounts := New().Bindings(home)

	want := home + "/.local/state/opencode"
	for _, m := range mounts {
		if m.Source == want {
			if m.Target != "" {
				t.Errorf("state tree: Target=%q, want \"\" (same-path mount)", m.Target)
			}
			if m.Mode != "rw" {
				t.Errorf("state tree: Mode=%q, want \"rw\" (model/UI state must persist back to host)", m.Mode)
			}
			if !m.Optional {
				t.Errorf("state tree: Optional=false, want true so a missing host dir is skipped")
			}
			return
		}
	}
	t.Errorf("no binding with Source=%q in %v", want, mounts)
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

// Host skills under ~/.claude/skills (bitbucket, jira, google-* etc.) must be
// surfaced individually as optional ro binds so opencode's skill
// auto-detection sees them, matching claude's own convention. Names that
// collide with an embedded skill are skipped -- the embedded bind is
// authoritative and a second mount at the same target would fail.
func TestBindings_HostSkillsBoundIndividually(t *testing.T) {
	withMissingOpencode(t)
	home := "/home/test"
	names := skills.EmbeddedSkillNames()
	if len(names) == 0 {
		t.Fatal("EmbeddedSkillNames returned empty; nothing to test")
	}
	embeddedName := names[0]

	withFakeSkillsDir(t, []fs.DirEntry{
		fakeDirEntry{name: "bitbucket", isDir: true},
		fakeDirEntry{name: "jira", isDir: true},
		fakeDirEntry{name: embeddedName, isDir: true},
	}, nil)

	mounts := New().Bindings(home)

	for _, name := range []string{"bitbucket", "jira"} {
		wantSrc := home + "/.claude/skills/" + name
		found := false
		for _, m := range mounts {
			if m.Source == wantSrc {
				found = true
				if m.Target != "" {
					t.Errorf("host skill %q: Target=%q, want \"\" (same-path mount)", name, m.Target)
				}
				if m.Mode != "" {
					t.Errorf("host skill %q: Mode=%q, want \"\" (ro)", name, m.Mode)
				}
				if !m.Optional {
					t.Errorf("host skill %q: Optional=false, want true", name)
				}
			}
		}
		if !found {
			t.Errorf("host skill %q: no binding with Source=%q in %v", name, wantSrc, mounts)
		}
	}

	// The entry colliding with an embedded skill name must not produce a
	// second mount at the host path -- the embedded bind is authoritative.
	collidingSrc := home + "/.claude/skills/" + embeddedName
	for _, m := range mounts {
		if m.Source == collidingSrc {
			t.Errorf("embedded-colliding skill %q: unexpected extra binding %+v", embeddedName, m)
		}
	}
}

// A ReadDir failure (e.g. ~/.claude/skills absent) must not disturb the
// existing embedded-skill bindings -- it should just yield zero additional
// host-skill bindings.
func TestBindings_SkillsDirReadErrorLeavesExistingBindingsIntact(t *testing.T) {
	withMissingOpencode(t)
	home := "/home/test"
	withFakeSkillsDir(t, nil, errors.New("test stub: read dir failed"))

	mounts := New().Bindings(home)

	prefix := home + "/.claude/skills/"
	names := skills.EmbeddedSkillNames()
	embeddedTargets := map[string]bool{}
	for _, name := range names {
		embeddedTargets[prefix+name] = true
	}

	for _, m := range mounts {
		if m.Source != "" && len(m.Source) > len(prefix) && m.Source[:len(prefix)] == prefix {
			if !embeddedTargets[m.Source] {
				t.Errorf("unexpected host-skill binding present despite ReadDir error: %+v", m)
			}
		}
	}
}

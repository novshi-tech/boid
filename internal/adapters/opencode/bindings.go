package opencode

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/skills"
)

// resolveCommand is overridable for tests; see internal/adapters/codex/bindings.go
// for the rationale (PATH-resolve then chase symlinks).
var resolveCommand = func(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// readSkillsDir is overridable for tests.
var readSkillsDir = os.ReadDir

// Bindings declares the host bind-mounts opencode.Adapter.Run() needs inside
// the sandbox. opencode keeps state in four trees:
//
//   - ~/.opencode/                 — package install + bin/ (rw, node_modules)
//   - ~/.config/opencode/          — opencode.jsonc config + node_modules (rw)
//   - ~/.local/share/opencode/     — auth.json, sqlite, repos snapshot (rw)
//   - ~/.local/state/opencode/     — model.json (selected model), kv.json (UI
//     settings), frecency/prompt history (rw)
//
// The state tree matters for parity with the host: opencode persists the
// most-recently-selected model in ~/.local/state/opencode/model.json and picks
// its default from that "recent" list at startup. Without this bind the sandbox
// opencode can't see it and falls back to a built-in default, so the model and
// UI settings the user chose on the host silently don't apply inside boid.
//
// The resolved binary parent dir is added on top so a plain `opencode` on
// PATH (e.g. ~/.local/bin/opencode dropped by a packaged install) lands
// inside the sandbox under the same path the host sees.
//
// Embedded skills are surfaced at ~/.claude/skills/<name> — opencode
// auto-detects skills under ~/.claude/ (same convention claude itself uses),
// so the bootstrap prompt can reference one canonical path across harnesses.
// See codex/bindings.go for the full rationale.
//
// All entries are Optional so a missing source on the host is silently
// skipped; the dispatcher converts Optional → shell-level if-guard.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	// Target is left empty: the dispatcher mounts these at the same path
	// inside the sandbox (and explicitly skips bindings whose explicit
	// Target matches Source, so the empty form is required, not optional).
	out := []adapters.BindMount{
		{
			Source:   homeDir + "/.opencode",
			Mode:     "rw",
			Optional: true,
		},
		{
			Source:   homeDir + "/.config/opencode",
			Mode:     "rw",
			Optional: true,
		},
		{
			Source:   homeDir + "/.local/share/opencode",
			Mode:     "rw",
			Optional: true,
		},
		{
			Source:   homeDir + "/.local/state/opencode",
			Mode:     "rw",
			Optional: true,
		},
	}
	if real, err := resolveCommand("opencode"); err == nil {
		out = append(out, adapters.BindMount{
			Source:   filepath.Dir(real),
			Optional: true,
		})
	}
	// Embedded skills live at ~/.local/share/boid/skills/<name> on the host
	// and are surfaced inside the sandbox at ~/.claude/skills/<name>.
	// opencode auto-detects skills there, mirroring claude's convention; the
	// task hook bootstrap prompt references the same canonical path across
	// claude / codex / opencode.
	skillsBase := homeDir + "/.local/share/boid/skills"
	embedded := make(map[string]bool, len(skills.EmbeddedSkillNames()))
	for _, name := range skills.EmbeddedSkillNames() {
		embedded[name] = true
		out = append(out, adapters.BindMount{
			Source:   skillsBase + "/" + name,
			Target:   homeDir + "/.claude/skills/" + name,
			Optional: true,
		})
	}
	// Host skills under ~/.claude/skills (bitbucket, jira, google-* etc.) are
	// surfaced individually as optional ro binds, one entry per subdirectory,
	// so opencode's skill auto-detection sees them too. This must NOT be a
	// single bind of the whole ~/.claude/skills directory: the sandbox layers
	// mounts onto a tmpfs root, and the embedded skill binds above land at
	// ~/.claude/skills/<name> — mounting the parent dir ro either shadows
	// those embedded binds (if mounted after) or makes their target
	// MkdirAll fail with EROFS (if mounted before, since the parent is
	// already read-only). Per-entry binds sidestep both failure modes by
	// mounting siblings rather than nesting inside each other. Entries that
	// collide with an embedded skill name are skipped — the embedded bind
	// above is authoritative.
	hostSkillsDir := homeDir + "/.claude/skills"
	if entries, err := readSkillsDir(hostSkillsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || embedded[entry.Name()] {
				continue
			}
			out = append(out, adapters.BindMount{
				Source:   hostSkillsDir + "/" + entry.Name(),
				Optional: true,
			})
		}
	}
	return out
}

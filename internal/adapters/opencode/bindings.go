package opencode

import (
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

// Bindings declares the host bind-mounts opencode.Adapter.Run() needs inside
// the sandbox. opencode keeps state in three trees:
//
//   - ~/.opencode/                 — package install + bin/ (rw, node_modules)
//   - ~/.config/opencode/          — opencode.jsonc config + node_modules (rw)
//   - ~/.local/share/opencode/     — auth.json, sqlite, repos snapshot (rw)
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
	for _, name := range skills.EmbeddedSkillNames() {
		out = append(out, adapters.BindMount{
			Source:   skillsBase + "/" + name,
			Target:   homeDir + "/.claude/skills/" + name,
			Optional: true,
		})
	}
	return out
}

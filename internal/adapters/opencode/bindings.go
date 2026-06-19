package opencode

import (
	"os/exec"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/adapters"
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
	return out
}

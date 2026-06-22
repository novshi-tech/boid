package codex

import (
	"os/exec"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/skills"
)

// resolveCommand is overridable for tests. It mirrors the recipe in the
// HarnessAdapter.Bindings contract: PATH-resolve then chase symlinks so the
// returned path is a real file the sandbox can bind 1:1.
var resolveCommand = func(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// Bindings declares the host bind-mounts codex.Adapter.Run() needs inside
// the sandbox. Three concerns, each handled by one entry:
//
//  1. ~/.codex/ — rw state dir (sessions, sqlite, auth.json, config.toml).
//  2. The parent dir of the resolved `codex` binary on the host PATH. We
//     resolve symlinks first so a volta-shimmed install (~/.volta/bin/codex
//     → volta-shim) lands on the shim's own dir, not on a dangling link.
//     The dispatcher's buildPATH automatically lifts this dir onto PATH.
//  3. ~/.volta/ — ro shim runtime tree. volta-shim execs binaries from
//     ~/.volta/tools/ under the hood, so binding only the bin dir is not
//     enough. Optional means non-volta hosts silently skip this entry.
//  4. ~/.local/share/boid/skills/<name> → ~/.boid/skills/<name> per embedded
//     skill. codex has no slash-command / skill loader, so the task hook
//     bootstrap prompt (see run.go taskBootstrapPrompt) tells the agent to
//     read ~/.boid/skills/boid-task/SKILL.md via its read-file tool. The
//     bind target is intentionally distinct from claude's ~/.claude/skills/
//     mount so the two harnesses do not collide.
//
// All entries are Optional: missing source paths just drop out of the mount
// set instead of failing the dispatch. That keeps a host without codex
// installed (e.g. CI) from breaking dispatch unrelated to the codex agent.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	// Target is left empty: the dispatcher mounts these at the same path
	// inside the sandbox (and explicitly skips bindings whose explicit
	// Target matches Source, so the empty form is required, not optional).
	out := []adapters.BindMount{
		{
			Source:   homeDir + "/.codex",
			Mode:     "rw",
			Optional: true,
		},
	}
	if real, err := resolveCommand("codex"); err == nil {
		out = append(out, adapters.BindMount{
			Source:   filepath.Dir(real),
			Optional: true,
		})
	}
	out = append(out, adapters.BindMount{
		Source:   homeDir + "/.volta",
		Optional: true,
	})
	// Embedded skills live at ~/.local/share/boid/skills/<name> on the host
	// and are surfaced inside the sandbox at ~/.boid/skills/<name> so the
	// task hook bootstrap prompt can reference a stable path independent of
	// the harness binary's own data dir.
	skillsBase := homeDir + "/.local/share/boid/skills"
	for _, name := range skills.EmbeddedSkillNames() {
		out = append(out, adapters.BindMount{
			Source:   skillsBase + "/" + name,
			Target:   homeDir + "/.boid/skills/" + name,
			Optional: true,
		})
	}
	return out
}

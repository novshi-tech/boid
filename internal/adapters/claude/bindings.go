package claude

import (
	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/skills"
)

// Bindings declares the host bind-mounts claude.Adapter.Run() needs inside
// the sandbox. They replace the bindings that boid-kits' claude-code/kit.yaml
// used to declare; once Phase 3-d retires that kit the dispatcher no longer
// reads it at all on the claude path.
//
// Layout (Phase 3-c):
//   - ~/.local/bin               (ro, dir) — claude CLI directory; also bin → PATH
//   - ~/.local/share/claude      (ro, dir) — claude shared data
//   - ~/.claude                  (rw, dir) — claude config / state
//   - ~/.claude.json             (rw, file) — claude main settings
//   - ~/.local/share/boid/skills/<name> → ~/.claude/skills/<name> per embedded
//     skill so /boid-supervisor / /boid-executor / ... resolve inside claude.
//
// All entries are Optional so a missing source on the host is silently
// skipped (the dispatcher converts Optional → shell-level if-guard, matching
// the previous dirGuardExpr / existsGuardExpr behaviour).
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	// Leave Target empty when it equals Source — the dispatcher's
	// additionalBindingMounts() skips any binding whose explicit Target
	// matches its Source (a guard against worktree self-mounts). The empty
	// form is the canonical "same path inside the sandbox" recipe.
	out := []adapters.BindMount{
		{
			Source:   homeDir + "/.local/bin",
			Optional: true,
		},
		{
			Source:   homeDir + "/.local/share/claude",
			Optional: true,
		},
		{
			Source:   homeDir + "/.claude",
			Mode:     "rw",
			Optional: true,
		},
		{
			Source:   homeDir + "/.claude.json",
			Mode:     "rw",
			IsFile:   true,
			Optional: true,
		},
	}
	// Embedded skills *do* need a distinct Target (the host path is under
	// ~/.local/share/boid/skills/<name> but inside claude they have to live
	// at ~/.claude/skills/<name>), so Target is set explicitly here.
	skillsBase := homeDir + "/.local/share/boid/skills"
	for _, name := range skills.EmbeddedSkillNames() {
		src := skillsBase + "/" + name
		out = append(out, adapters.BindMount{
			Source:   src,
			Target:   homeDir + "/.claude/skills/" + name,
			Optional: true,
		})
	}
	return out
}

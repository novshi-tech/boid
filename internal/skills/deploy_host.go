package skills

import "embed"

// hostSkillsFS embeds the host-facing skill set: SKILL.md files meant for
// ~/.claude/skills/ on the machine that runs the `boid` CLI directly (not
// inside a job sandbox). `boid install-skills` is the only caller.
//
// Deliberately a SEPARATE embed.FS from skillsFS (deploy.go), never merged
// with it. skillsFS/DeployAll/EmbeddedSkillNames are load-bearing for
// internal/dispatcher's per-job sandbox bind list
// (skills_overlay.go:157,199, sandbox_builder.go:1286) — a host skill
// documents `boid workspace`/`boid web pair`/etc. host CLI operations that
// make no sense, and may not even be reachable, from inside a job
// container. Keeping the two embed.FS values and their entry points
// entirely distinct is what makes that leak structurally impossible rather
// than a convention someone has to remember. See
// TestDeployHostSkills_DoesNotAffectSandboxSkillSet in deploy_host_test.go
// for the regression guard.
//
//go:embed hostdata/boid-cli-workspace hostdata/boid-cli-daemon hostdata/boid-cli-task
var hostSkillsFS embed.FS

// DeployHostSkills extracts the embedded host skill directories under
// baseDir, same idempotent/content-diff-only-write contract as DeployAll
// (see deploy.go's doc comment) — it shares the same underlying
// deployAllFrom/deploySkillDir machinery, just pointed at hostSkillsFS's
// "hostdata" root instead of skillsFS's "data" root.
func DeployHostSkills(baseDir string) error {
	return deployAllFrom(hostSkillsFS, "hostdata", baseDir)
}

// HostSkillNames returns the slugs of the embedded host skill directories in
// stable lexical order. Used only by `boid install-skills` to report what it
// installed — unlike EmbeddedSkillNames, nothing in internal/dispatcher
// consults this.
func HostSkillNames() []string {
	entries, err := hostSkillsFS.ReadDir("hostdata")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

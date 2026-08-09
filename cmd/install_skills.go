package cmd

// cmd/install_skills.go implements `boid install-skills`: materializes the
// host-facing skill set (boid-cli-workspace / boid-cli-daemon / boid-cli-task,
// internal/skills/hostdata/) into ~/.claude/skills/ so a Claude Code session
// running directly on the machine that has `boid` installed — NOT inside a
// job sandbox — knows how to operate boid's own workspaces/projects,
// daemon/web pairing, and tasks/jobs from the outside.
//
// Pure local filesystem operation (skills.DeployHostSkills), never talks to
// the daemon — same axis as `boid check`/`boid fetch`, hence
// annotationSkipAutostart=skip + scopeLocal.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/novshi-tech/boid/internal/skills"
)

var installSkillsCmd = &cobra.Command{
	Use:   "install-skills",
	Short: "Install boid CLI operation skills into ~/.claude/skills/",
	Long: "Materializes the host-facing boid-cli-* skills (workspace/project,\n" +
		"daemon/web, task/job operations) so a Claude Code session run directly\n" +
		"on this machine — outside any boid job sandbox — knows how to drive\n" +
		"the boid CLI. Idempotent: re-running only rewrites files whose content\n" +
		"actually changed.",
	SilenceUsage: true,
	RunE:         runInstallSkills,
}

func init() {
	installSkillsCmd.Flags().String("dir", "", "target directory (default: ~/.claude/skills)")
	installSkillsCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	}
	rootCmd.AddCommand(installSkillsCmd)
}

func runInstallSkills(cmd *cobra.Command, _ []string) error {
	dir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return err
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		dir = filepath.Join(home, ".claude", "skills")
	}
	dir, err = resolveInstallDir(dir)
	if err != nil {
		return fmt.Errorf("resolve target directory %q: %w", dir, err)
	}

	if err := skills.DeployHostSkills(dir); err != nil {
		return fmt.Errorf("install skills into %q: %w", dir, err)
	}

	names := skills.HostSkillNames()
	return renderOutput(cmd, names, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "installed %d skill(s) into %s:\n", len(names), dir)
		for _, name := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", name)
		}
		return nil
	})
}

// resolveInstallDir turns dir (whatever the user typed, or the computed
// ~/.claude/skills default) into an absolute path with every EXISTING
// ancestor component symlink-resolved, then returns.
//
// Two reasons this can't just hand dir straight to skills.DeployHostSkills:
//
//  1. skills.DeployHostSkills → openBaseDirSafe requires an absolute path
//     (its openat2 walk starts at "/"); a relative --dir would otherwise
//     fail deep inside that call with an error that reads like a
//     symlink-attack rejection rather than "pass an absolute path".
//  2. openBaseDirSafe refuses to walk through ANY symlinked path component
//     — a threat model that matters when baseDir is job-writable storage
//     (DeployAll's original caller), but ~/.claude/skills here is neither
//     job-reachable nor attacker-controlled. A perfectly ordinary dotfiles
//     layout (`~/.claude -> ~/dotfiles/claude`, chezmoi/stow-style) would
//     otherwise make this command permanently fail for a reason that has
//     nothing to do with the security property openBaseDirSafe exists to
//     enforce. Pre-resolving existing symlinks here — in a context that
//     trusts the local filesystem — sidesteps that without weakening
//     openBaseDirSafe's guarantee for its original (job-writable) caller.
//
// Components that don't exist yet (e.g. "skills" under an existing
// "~/.claude") are left as-is and joined back on: they can't be symlinks if
// they don't exist, and skills.DeployHostSkills creates them as real
// directories.
func resolveInstallDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return resolveExistingAncestors(abs), nil
}

// resolveExistingAncestors resolves the longest existing prefix of path
// through filepath.EvalSymlinks and rejoins whatever trailing components
// don't exist yet, unresolved (see resolveInstallDir's doc comment for why).
func resolveExistingAncestors(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		// Reached the filesystem root without finding an existing
		// component; nothing left to resolve.
		return path
	}
	return filepath.Join(resolveExistingAncestors(parent), filepath.Base(path))
}

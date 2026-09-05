//go:build linux

package cmd

// cmd/install_skills.go implements `boid install-skills`: materializes the
// host-facing skill set into ~/.claude/skills/ so a Claude Code session
// running directly on the machine — NOT inside a job sandbox — knows how
// to operate boid's own workspaces/projects, daemon/web pairing, and
// tasks/jobs from the outside.
//
// Pure local filesystem operation, never talks to the daemon.
//
// Linux-only: internal/skills/safe_deploy.go hardens the deploy against
// TOCTOU with openat-style primitives that have no portable equivalent (see
// docs/plans/windows-client-build.md for the tradeoff this implies).
// TestNoAccidentallyLinuxOnlyRemoteCommands does not flag this file because
// it only guards scope=remote commands, and this one is scope=local.

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

// resolveInstallDir turns dir into an absolute path with every EXISTING
// ancestor component symlink-resolved, before handing it to
// skills.DeployHostSkills.
//
// skills.DeployHostSkills → openBaseDirSafe requires an absolute path and
// refuses to walk through any symlinked path component — appropriate when
// baseDir is job-writable storage, but it would also reject an ordinary
// dotfiles layout (`~/.claude -> ~/dotfiles/claude`) here, where the target
// is neither job-reachable nor attacker-controlled. Pre-resolving existing
// symlinks in this trusted-local-filesystem context sidesteps that without
// weakening openBaseDirSafe's guarantee for its original caller.
//
// Components that don't exist yet are left unresolved and joined back on:
// they can't be symlinks if they don't exist.
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

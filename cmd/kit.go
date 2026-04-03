package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/client"
	kit "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var kitCmd = &cobra.Command{
	Use:   "kit",
	Short: "Manage kits",
}

var kitInstallSSH bool

var kitInstallCmd = &cobra.Command{
	Use:   "install [repo]",
	Short: "Install kit repositories",
	Long:  "Install a kit repository. Without arguments, installs all remote kit repos referenced by the current project.",
	Args:  cobra.RangeArgs(0, 1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return kitInstallSingle(args[0])
		}
		return kitInstallFromProject()
	},
}

func kitInstallSingle(repoRef string) error {
	reg := kit.NewRegistry(defaultKitsDir())
	if err := reg.Install(repoRef, kitInstallSSH); err != nil {
		return err
	}
	fmt.Printf("installed: %s\n", repoRef)
	reloadProjects()
	return nil
}

func kitInstallFromProject() error {
	projectDir, err := resolveProjectRoot("")
	if err != nil {
		return err
	}

	meta, err := kit.ReadProjectMeta(projectDir)
	if err != nil {
		return err
	}

	kitRefs := meta.Kits

	local, err := kit.ReadProjectLocalMeta(projectDir)
	if err != nil {
		return err
	}
	if local != nil {
		kitRefs, err = kit.EffectiveKitRefs(meta.Kits, local.Kits)
		if err != nil {
			return err
		}
	}

	kitRefStrs := make([]string, len(kitRefs))
	for i, r := range kitRefs {
		kitRefStrs[i] = r.Ref
	}
	repos := kit.RepoRefsFromKitRefs(kitRefStrs)
	if len(repos) == 0 {
		fmt.Println("no remote kit repos to install")
		return nil
	}

	reg := kit.NewRegistry(defaultKitsDir())
	installed := false
	for _, repo := range repos {
		if reg.IsInstalled(repo) {
			fmt.Printf("already installed: %s\n", repo)
			continue
		}
		if err := reg.Install(repo, kitInstallSSH); err != nil {
			return err
		}
		fmt.Printf("installed: %s\n", repo)
		installed = true
	}
	if installed {
		reloadProjects()
	}
	return nil
}

func reloadProjects() {
	c := client.NewUnixClient(client.DefaultSocketPath())
	if err := c.Do("POST", "/api/projects/reload", nil, nil); err != nil {
		return
	}
	fmt.Println("projects reloaded")
}

var kitListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed kit repositories",
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := kit.NewRegistry(defaultKitsDir())
		repos, err := reg.List()
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			fmt.Println("no kits installed")
			return nil
		}
		for _, r := range repos {
			fmt.Println(r)
		}
		return nil
	},
}

var kitRemoveCmd = &cobra.Command{
	Use:   "remove <repo>",
	Short: "Remove an installed kit repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := kit.NewRegistry(defaultKitsDir())
		if err := reg.Remove(args[0]); err != nil {
			return err
		}
		fmt.Printf("removed: %s\n", args[0])
		return nil
	},
}

var kitUpdateCmd = &cobra.Command{
	Use:   "update <repo>",
	Short: "Update an installed kit repository (git pull)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := kit.NewRegistry(defaultKitsDir())
		if err := reg.Update(args[0]); err != nil {
			return err
		}
		fmt.Printf("updated: %s\n", args[0])
		return nil
	},
}

func init() {
	kitInstallCmd.Flags().BoolVar(&kitInstallSSH, "ssh", false, "Use SSH protocol (git@host:path) instead of HTTPS")
	kitCmd.AddCommand(kitInstallCmd, kitListCmd, kitRemoveCmd, kitUpdateCmd)
	rootCmd.AddCommand(kitCmd)
}

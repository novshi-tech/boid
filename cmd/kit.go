package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/kit"
	"github.com/spf13/cobra"
)

var kitCmd = &cobra.Command{
	Use:   "kit",
	Short: "Manage kits",
}

var kitInstallCmd = &cobra.Command{
	Use:   "install <repo>",
	Short: "Install a kit repository (e.g. github.com/user/repo)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := kit.NewRegistry(defaultKitsDir())
		if err := reg.Install(args[0]); err != nil {
			return err
		}
		fmt.Printf("installed: %s\n", args[0])
		return nil
	},
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
	kitCmd.AddCommand(kitInstallCmd, kitListCmd, kitRemoveCmd, kitUpdateCmd)
	rootCmd.AddCommand(kitCmd)
}

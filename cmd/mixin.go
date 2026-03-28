package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/mixin"
	"github.com/spf13/cobra"
)

var mixinCmd = &cobra.Command{
	Use:   "mixin",
	Short: "Manage mixins",
}

var mixinInstallCmd = &cobra.Command{
	Use:   "install <repo>",
	Short: "Install a mixin repository (e.g. github.com/user/repo)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := mixin.NewRegistry(defaultMixinsDir())
		if err := reg.Install(args[0]); err != nil {
			return err
		}
		fmt.Printf("installed: %s\n", args[0])
		return nil
	},
}

var mixinListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed mixin repositories",
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := mixin.NewRegistry(defaultMixinsDir())
		repos, err := reg.List()
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			fmt.Println("no mixins installed")
			return nil
		}
		for _, r := range repos {
			fmt.Println(r)
		}
		return nil
	},
}

var mixinRemoveCmd = &cobra.Command{
	Use:   "remove <repo>",
	Short: "Remove an installed mixin repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := mixin.NewRegistry(defaultMixinsDir())
		if err := reg.Remove(args[0]); err != nil {
			return err
		}
		fmt.Printf("removed: %s\n", args[0])
		return nil
	},
}

var mixinUpdateCmd = &cobra.Command{
	Use:   "update <repo>",
	Short: "Update an installed mixin repository (git pull)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := mixin.NewRegistry(defaultMixinsDir())
		if err := reg.Update(args[0]); err != nil {
			return err
		}
		fmt.Printf("updated: %s\n", args[0])
		return nil
	},
}

func init() {
	mixinCmd.AddCommand(mixinInstallCmd, mixinListCmd, mixinRemoveCmd, mixinUpdateCmd)
	rootCmd.AddCommand(mixinCmd)
}

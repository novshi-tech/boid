package cmd

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	orchestrator "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var kitCmd = &cobra.Command{
	Use:   "kit",
	Short: "Manage kits",
}

// kitInitCmd is a stub for the kit init command. The full implementation
// (environment scan + kit.yaml generation) will be added in a subsequent PR.
//
// When the full implementation dispatches a sandbox-based generation script,
// the JobSpec must set:
//   SandboxProfile: int(sandbox.ProfileInit)
// This causes BuildPlan to mount the entire host root read-only (so the
// generation script can detect installed tools) and skips broker registration
// / socket mount (init scripts do not invoke boid host-commands).
var kitInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate kit.yaml for this machine (stub — full implementation in a future PR)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("boid kit init: 生成スキルは今後の PR で実装予定です")
		return nil
	},
}

var kitListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed kits",
	RunE: func(cmd *cobra.Command, args []string) error {
		reg := orchestrator.NewRegistry(defaultKitsDir())
		names, err := reg.List()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("no kits installed")
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	},
}

var kitRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove an installed kit",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Early validation, consistent with the workspace slug 3-layer
		// defense: fail fast on path-traversal / invalid characters so we
		// never filepath.Join an attacker-controlled value.
		if err := orchestrator.ValidKitName(name); err != nil {
			return err
		}

		// Check if any workspace references this kit.
		wsStore := orchestrator.NewWorkspaceStore("")
		slug, checkErr := workspacesReferencingKit(wsStore, name)
		if checkErr != nil {
			return fmt.Errorf("check workspace references: %w", checkErr)
		}
		if len(slug) > 0 {
			return fmt.Errorf("kit %q is referenced by workspace(s): %s\nRemove the kit from those workspaces first", name, strings.Join(slug, ", "))
		}

		reg := orchestrator.NewRegistry(defaultKitsDir())
		if err := reg.Remove(name); err != nil {
			return err
		}
		fmt.Printf("removed: %s\n", name)
		return nil
	},
}

// workspacesReferencingKit returns the slugs of workspaces whose Kits field
// contains the given kit name.
func workspacesReferencingKit(store *orchestrator.WorkspaceStore, kitName string) ([]string, error) {
	slugs, err := store.List()
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, slug := range slugs {
		ws, err := store.Load(slug)
		if err != nil {
			continue // skip unloadable workspaces
		}
		for _, k := range ws.Kits {
			if k == kitName {
				refs = append(refs, slug)
				break
			}
		}
	}
	return refs, nil
}

func reloadProjects() {
	c := client.NewUnixClient(client.DefaultSocketPath())
	if err := c.Do("POST", "/api/projects/reload", nil, nil); err != nil {
		return
	}
	fmt.Println("projects reloaded")
}

func init() {
	kitCmd.AddCommand(kitInitCmd, kitListCmd, kitRemoveCmd)
	rootCmd.AddCommand(kitCmd)
}

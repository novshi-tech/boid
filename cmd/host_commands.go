package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// host_commands.go implements `boid host-commands list` / `boid
// host-commands reload` (docs/plans/workspace-db-consolidation.md PR4 Step
// I, decision 15): the CLI counterpart of GET /api/host_commands and
// POST /api/host_commands/reload. There is no create/edit subcommand — per
// the plan doc, the aggregated ~/.config/boid/host_commands.yaml is edited
// by hand, and `reload` is only how the daemon is told to re-read it.

var hostCommandsCmd = &cobra.Command{
	Use:   "host-commands",
	Short: "Inspect and reload the daemon's aggregated host_commands config",
}

var hostCommandsListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List host_commands known to the daemon (GET /api/host_commands)",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runHostCommandsList,
}

var hostCommandsReloadCmd = &cobra.Command{
	Use:         "reload",
	Short:       "Reload ~/.config/boid/host_commands.yaml after a hand edit (POST /api/host_commands/reload)",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runHostCommandsReload,
}

func init() {
	hostCommandsCmd.AddCommand(hostCommandsListCmd, hostCommandsReloadCmd)
	rootCmd.AddCommand(hostCommandsCmd)
}

func runHostCommandsList(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	// MINOR 1 (codex review, docs/plans/workspace-db-consolidation.md):
	// GET /api/host_commands now returns a sorted name list (the plan doc's
	// "参照名一覧を返す契約"), not the full name -> spec definition map.
	var names []string
	if err := c.Do("GET", "/api/host_commands", nil, &names); err != nil {
		return fmt.Errorf("list host_commands: %w", err)
	}

	return renderOutput(cmd, names, func() error {
		out := cmd.OutOrStdout()
		if len(names) == 0 {
			fmt.Fprintln(out, "no host_commands configured")
			return nil
		}

		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME")
		for _, name := range names {
			fmt.Fprintln(tw, name)
		}
		return tw.Flush()
	})
}

func runHostCommandsReload(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())
	if err := c.Do("POST", "/api/host_commands/reload", nil, nil); err != nil {
		return fmt.Errorf("reload host_commands: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "host_commands reloaded")
	return nil
}

package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/version"
	"github.com/spf13/cobra"
)

// versionCmd prints the running boid binary's version identity. It never
// talks to the daemon: the identity comes entirely from internal/version.
//
// scopeNeutral (not scopeLocal): the result must not depend on which
// --profile the CLI resolves, nor block on that resolution failing.
// Autostart is skipped since a version print must not spin up a daemon.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the boid version",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeNeutral,
	},
	DisableFlagsInUseLine: true,
	SilenceUsage:          true,
	RunE:                  runVersion,
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func runVersion(cmd *cobra.Command, args []string) error {
	fmt.Fprintln(cmd.OutOrStdout(), formatVersionLine(version.Version()))
	return nil
}

// formatVersionLine renders v (from internal/version.Version()) as the
// single line `boid version` prints.
func formatVersionLine(v string) string {
	if v == "" {
		return "boid (version unknown)"
	}
	if version.IsExactRelease(v) {
		return fmt.Sprintf("boid %s", v)
	}
	return fmt.Sprintf("boid %s (local build)", v)
}

package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/version"
	"github.com/spf13/cobra"
)

// versionCmd prints the running boid binary's version identity
// (docs/plans/release-onboarding.md 穴2). It never talks to the daemon —
// the identity comes entirely from internal/version, which itself resolves
// either an image-build ldflags override or `go install`'s
// debug.ReadBuildInfo() module version — so this is scopeLocal with
// autostart skipped, mirroring `boid fetch`/`boid init`: a user should be
// able to check the CLI's own version with no daemon reachable at all.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the boid version",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	},
	DisableFlagsInUseLine: true,
	SilenceUsage:          true,
	RunE:                  runVersion,
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func runVersion(cmd *cobra.Command, args []string) error {
	v := version.Version()
	if v == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "boid (version unknown)")
		return nil
	}
	if version.IsExactRelease(v) {
		fmt.Fprintf(cmd.OutOrStdout(), "boid %s\n", v)
		return nil
	}
	// Not an exact release tag: 穴2's rule treats pseudo-versions, "+dirty"
	// suffixes and "(devel)" all alike as a local build, so say so plainly
	// rather than implying this is a shippable release identity.
	fmt.Fprintf(cmd.OutOrStdout(), "boid %s (local build)\n", v)
	return nil
}

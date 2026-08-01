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
// debug.ReadBuildInfo() module version.
//
// scopeNeutral, not scopeLocal (codex review of this PR): scopeLocal is
// rejected by root's PersistentPreRunE whenever an https profile is
// selected (cmd/root.go's isLocalScope check, decision 6 of
// docs/plans/cli-remote-connection.md) — appropriate for a command whose
// result would be silently wrong against the wrong host, but wrong here.
// "What version of the CLI am I running" has nothing to do with which
// daemon --profile happens to point at, and its result should not depend
// on profile resolution having gone well: scopeNeutral means a profile
// resolution failure never blocks the command (see PersistentPreRunE's
// isNeutralScope branches), not that no resolution is attempted at all —
// on the happy path the same PersistentPreRunE that every command shares
// still resolves the active profile (and, for an https one, its token)
// before RunE runs; this command's RunE simply never looks at any of that
// (see cmd/login.go and root.go's scopeNeutral doc comment for the same
// contract applied to login/logout). autostart is still skipped: a version
// print must not spin up a daemon.
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

// formatVersionLine renders v — the output of internal/version.Version() —
// as the single line `boid version` prints. Split out from runVersion so
// the three cases 穴2's rule distinguishes (exact release tag / anything
// else / no version info at all) are unit-testable without needing to
// override internal/version's own package state from this package
// (cmd/version_test.go's TestFormatVersionLine).
func formatVersionLine(v string) string {
	if v == "" {
		return "boid (version unknown)"
	}
	if version.IsExactRelease(v) {
		return fmt.Sprintf("boid %s", v)
	}
	// Not an exact release tag: 穴2's rule treats pseudo-versions, "+dirty"
	// suffixes and "(devel)" all alike as a local build, so say so plainly
	// rather than implying this is a shippable release identity.
	return fmt.Sprintf("boid %s (local build)", v)
}

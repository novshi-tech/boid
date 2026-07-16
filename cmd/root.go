package cmd

import (
	"context"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// annotationSkipAutostart is the cobra annotation key used to opt a command
// out of automatic server startup. Set the value to "skip" on commands that
// must not trigger EnsureRunning (e.g. start, stop, gc).
const annotationSkipAutostart = "boid.autostart"

// scopeAnnotationKey is the cobra annotation key every leaf command must
// carry (docs/plans/workspace-db-consolidation.md decision 18, Phase 3 CLI
// リモート pre-requisite): which of scopeRemote/scopeLocal/scopeNeutral it
// belongs to. This is a distinct concern from annotationSkipAutostart above
// — that one is about whether invoking the command should try to launch the
// daemon first; this one is about whether the command's own work happens
// through the daemon's HTTP API at all (relevant once Phase 3 lets the CLI
// target a *remote* daemon over the network instead of always the local
// UNIX socket). The two annotations use different keys and coexist without
// conflict; a command can be, and several are, "remote scope, but also
// annotationSkipAutostart=skip" (e.g. gc: it talks to the API but should not
// spin up a daemon just to immediately garbage-collect it).
//
// cmd/scope_annotations_test.go enforces that every leaf command sets this
// to one of the three values below — an unclassified command is a build
// failure (fail-closed), not silently defaulted.
const scopeAnnotationKey = "boid.scope"

const (
	// scopeRemote marks a command whose work happens through the daemon's
	// HTTP API (today always the local UNIX socket; Phase 3 makes this
	// potentially a remote daemon over the network).
	scopeRemote = "remote"
	// scopeLocal marks a command that completes entirely without a daemon
	// — it either never talks to one (e.g. `kit list`, which reads
	// ~/.local/share/boid/kits directly) or *is* daemon lifecycle machinery
	// itself (start/stop) rather than a client of it.
	scopeLocal = "local"
	// scopeNeutral marks a command that requires no profile resolution at
	// all (docs/plans/cli-remote-connection.md: `login`/`logout`, not yet
	// implemented as of this writing — no shipped command currently uses
	// this value). `check` used to be cited here as the example (it works
	// standalone but also opportunistically talks to the daemon when one
	// happens to be reachable), but codex review round 2
	// (docs/plans/workspace-db-consolidation.md MAJOR 3) reclassified it to
	// scopeLocal to match the plan doc's classification table — see
	// cmd/check.go's annotation comment for the reasoning.
	scopeNeutral = "neutral"
)

var rootCmd = &cobra.Command{
	Use:   "boid",
	Short: "Personal AI orchestrator",
	// PersistentPreRunE is inherited by all subcommands and ensures the boid
	// server is running before any command that requires a socket connection.
	// Commands (or any ancestor command) annotated with boid.autostart=skip
	// bypass this check.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if isCompletionInvocation(cmd) {
			return nil
		}
		for c := cmd; c != nil; c = c.Parent() {
			if c.Annotations[annotationSkipAutostart] == "skip" {
				return nil
			}
		}
		return client.EnsureRunning(context.Background())
	},
}

func init() {
	rootCmd.PersistentFlags().StringP("output", "o", "plain", "Output format: plain, json, yaml")
}

func Execute() error {
	return rootCmd.Execute()
}

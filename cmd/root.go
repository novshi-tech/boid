package cmd

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/tui"
	"github.com/spf13/cobra"
)

// annotationSkipAutostart is the cobra annotation key used to opt a command
// out of automatic server startup. Set the value to "skip" on commands that
// must not trigger EnsureRunning (e.g. start, stop, gc).
const annotationSkipAutostart = "boid.autostart"

var rootCmd = &cobra.Command{
	Use:   "boid",
	Short: "Personal AI orchestrator",
	// PersistentPreRunE is inherited by all subcommands and ensures the boid
	// server is running before any command that requires a socket connection.
	// Commands (or any ancestor command) annotated with boid.autostart=skip
	// bypass this check.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		for c := cmd; c != nil; c = c.Parent() {
			if c.Annotations[annotationSkipAutostart] == "skip" {
				return nil
			}
		}
		return client.EnsureRunning(context.Background())
	},
	RunE: runTUI,
}

func runTUI(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	tmuxEnabled := tui.InTmux()

	app := tui.NewApp(c, tmuxEnabled)
	p := tea.NewProgram(app, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	rootCmd.PersistentFlags().StringP("output", "o", "plain", "Output format: plain, json, yaml")
}

func Execute() error {
	return rootCmd.Execute()
}

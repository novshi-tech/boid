package cmd

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/tui"
	"github.com/spf13/cobra"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI",
	RunE:  runTUI,
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}

func runTUI(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	tmuxEnabled := tui.InTmux()

	app := tui.NewApp(c, tmuxEnabled)
	p := tea.NewProgram(app, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

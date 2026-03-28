package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/tmux"
	"github.com/spf13/cobra"
)

var shellCmd = &cobra.Command{
	Use:   "shell <project-id>",
	Short: "Open a sandbox shell for a project",
	Args:  cobra.ExactArgs(1),
	RunE:  runShell,
}

func init() {
	rootCmd.AddCommand(shellCmd)
}

func runShell(cmd *cobra.Command, args []string) error {
	projectID := args[0]
	t := &tmux.RealTmux{}
	session := "boid"
	windowName := fmt.Sprintf("shell-%s", projectID)

	if err := t.EnsureSession(session); err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}
	if err := t.NewWindow(session, windowName); err != nil {
		return fmt.Errorf("new window: %w", err)
	}
	return t.Attach(session)
}

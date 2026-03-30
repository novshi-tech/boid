package cmd

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/job"
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

	cfg, err := buildSandboxConfig(projectID)
	if err != nil {
		return err
	}
	cfg.JobID = fmt.Sprintf("shell-%s", projectID)
	cfg.Interactive = true

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		return fmt.Errorf("write sandbox scripts: %w", err)
	}

	t := &tmux.RealTmux{}
	session := "boid"
	windowName := fmt.Sprintf("shell-%s", projectID)

	if err := t.EnsureSession(session); err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}
	if err := t.RunInWindow(session, windowName, fmt.Sprintf("bash %s", outerPath)); err != nil {
		return fmt.Errorf("run in window: %w", err)
	}

	// If already inside tmux, switch to the new window; otherwise attach.
	if os.Getenv("TMUX") != "" {
		return t.SwitchClient(session, windowName)
	}
	return t.Attach(session)
}

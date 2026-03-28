package cmd

import (
	"github.com/novshi-tech/boid/internal/tmux"
	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach to the boid tmux session",
	RunE:  runAttach,
}

func init() {
	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	t := &tmux.RealTmux{}
	return t.Attach("boid")
}

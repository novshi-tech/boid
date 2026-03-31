package cmd

import (
	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
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
	t := &dtmux.RealTmux{}
	return t.Attach("boid")
}

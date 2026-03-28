package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "boid",
	Short: "Personal AI orchestrator",
}

func Execute() error {
	return rootCmd.Execute()
}

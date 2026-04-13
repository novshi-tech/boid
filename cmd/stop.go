package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the boid server",
	RunE:  runStop,
}

func init() {
	stopCmd.Annotations = map[string]string{annotationSkipAutostart: "skip"}
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var result map[string]string
	if err := c.Do("POST", "/api/shutdown", nil, &result); err != nil {
		return fmt.Errorf("stop server: %w", err)
	}
	fmt.Println("server stopped")
	return nil
}

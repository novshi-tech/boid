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
	stopCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		// scopeLocal (per plan doc guidance): stop is daemon lifecycle
		// management, classified alongside start/status even though its
		// RunE happens to call the API to ask the daemon to shut itself
		// down.
		scopeAnnotationKey: scopeLocal,
	}
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

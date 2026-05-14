package cmd

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage running agents",
}

var agentStopCmd = &cobra.Command{
	Use:   "stop <job-id>",
	Short: "Signal the agent of a running job to stop (SIGUSR1 → claude SIGTERM)",
	Long: `Asks the daemon to deliver SIGUSR1 to the runtime's process group so
run-agent.py SIGTERMs only the claude process. bash and the EXIT trap stay
alive, so the trap's "boid job done --output-file payload_patch.json" remains
the canonical CompleteJob caller — preserving the session id (and any
artifact the agent wrote to payload_patch.json) through the broker token.`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentStop,
}

func init() {
	agentCmd.AddCommand(agentStopCmd)
	rootCmd.AddCommand(agentCmd)
}

func runAgentStop(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	c := client.NewUnixClient(client.DefaultSocketPath())
	var result map[string]any
	if err := c.Do("POST", fmt.Sprintf("/api/jobs/%s/agent-stop", jobID), map[string]any{}, &result); err != nil {
		return fmt.Errorf("agent stop: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "agent stop signalled for job %s\n", jobID)
	return nil
}

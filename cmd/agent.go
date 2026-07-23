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
the harness adapter SIGTERMs only the claude process. The go-native runner
keeps running and posts "boid job done" through the broker directly once the
child exits — the canonical CompleteJob caller — preserving the session id
through the broker token (any artifact the agent reported via
"boid task update --payload-patch" was already applied immediately, before
this point).`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runAgentStop,
}

func init() {
	agentCmd.AddCommand(agentStopCmd)
	rootCmd.AddCommand(agentCmd)
}

func runAgentStop(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	c := client.FromContext(cmd.Context())
	var result map[string]any
	if err := c.Do("POST", fmt.Sprintf("/api/jobs/%s/agent-stop", jobID), map[string]any{}, &result); err != nil {
		return fmt.Errorf("agent stop: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "agent stop signalled for job %s\n", jobID)
	return nil
}

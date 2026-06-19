package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// agent_session.go wires the Phase 3-d session-launching subcommands of
// `boid agent`. Each subcommand (claude / codex / opencode) is a thin
// front-end over POST /api/projects/{id}/sessions, then attaches to the
// resulting job's PTY so the harness CLI feels like a foreground process.
//
// Usage:
//
//	boid agent claude  -p <project> [--resume <session-id>] [--instruction "..."] [--readonly] [--model M]
//	boid agent codex    -p <project> [...]
//	boid agent opencode -p <project> [...]
//
// `boid agent stop <job-id>` (defined alongside in agent.go) is unchanged.

type agentSessionFlags struct {
	projectRef  string
	resumeID    string
	instruction string
	readonly    bool
	model       string
	displayName string
	noAttach    bool
}

func addAgentSessionFlags(cmd *cobra.Command, f *agentSessionFlags) {
	cmd.Flags().StringVarP(&f.projectRef, "project", "p", "", "project ref (id, name, or unique prefix); required")
	cmd.Flags().StringVar(&f.resumeID, "resume", "", "resume an existing session id instead of starting fresh")
	cmd.Flags().StringVar(&f.instruction, "instruction", "", "bootstrap prompt delivered as the first user turn (empty uses the harness default)")
	cmd.Flags().BoolVar(&f.readonly, "readonly", false, "mount the project workspace read-only (default: writable)")
	cmd.Flags().StringVar(&f.model, "model", "", "override the harness binary's default model")
	cmd.Flags().StringVar(&f.displayName, "name", "", "human-readable session label (default: \"<harness> session\")")
	cmd.Flags().BoolVar(&f.noAttach, "no-attach", false, "print the job id and exit instead of attaching to the PTY")
}

func init() {
	for _, harness := range []string{"claude", "codex", "opencode"} {
		h := harness // capture
		flags := &agentSessionFlags{}
		c := &cobra.Command{
			Use:   h + " -p <project>",
			Short: fmt.Sprintf("Start an interactive %s session for a project", h),
			Long: fmt.Sprintf(`Start a %s session under the daemon's sandbox for a project.

The session inherits the project's host_commands / additional_bindings /
env / secret_namespace traits and runs through internal/adapters/%s. The
command attaches to the resulting job's PTY unless --no-attach is set.`, h, h),
			Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				return runAgentSession(h, flags)
			},
		}
		addAgentSessionFlags(c, flags)
		agentCmd.AddCommand(c)
	}
}

func runAgentSession(harness string, flags *agentSessionFlags) error {
	if flags.projectRef == "" {
		return errors.New("--project is required")
	}
	c := client.NewUnixClient(client.DefaultSocketPath())

	// Resolve the project ref to its id so the URL path is canonical
	// (matches how /api/projects/{id}/sessions is mounted).
	project, err := resolveProjectRef(c, os.Stdin, os.Stderr, flags.projectRef)
	if err != nil {
		return fmt.Errorf("resolve project ref %q: %w", flags.projectRef, err)
	}
	projectID := project.ID

	req := api.StartSessionRequest{
		ProjectID:   projectID,
		HarnessType: harness,
		SessionID:   flags.resumeID,
		Instruction: flags.instruction,
		Readonly:    flags.readonly,
		Model:       flags.model,
		DisplayName: flags.displayName,
	}
	var result api.StartSessionResult
	if err := c.Do("POST", fmt.Sprintf("/api/projects/%s/sessions", projectID), req, &result); err != nil {
		return fmt.Errorf("start %s session: %w", harness, err)
	}

	// Emit the job id on stderr so users with --no-attach or piped stdout
	// always see it. (stdout in attach mode belongs to the PTY.)
	fmt.Fprintf(os.Stderr, "job_id=%s\n", result.JobID)
	if flags.noAttach {
		return nil
	}
	return attachToJob(result.JobID)
}

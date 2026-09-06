package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// agent_session.go wires the session-launching subcommands of `boid agent`
// (claude / codex / opencode): each is a thin front-end over POST
// /api/projects/{id}/sessions that attaches to the resulting job's PTY so
// the harness CLI feels like a foreground process. Sessions always start
// fresh (no resume). `boid agent shell` was retired in favor of `boid exec
// -p <project> -- bash`, which runs the same shell adapter.
//
// Usage:
//
//	boid agent claude  -p <project> [--instruction "..."] [--readonly] [--model M]
//	boid agent codex    -p <project> [...]
//	boid agent opencode -p <project> [...]
//
// `boid agent stop <job-id>` (defined alongside in agent.go) is unchanged.

type agentSessionFlags struct {
	projectRef  string
	instruction string
	readonly    bool
	model       string
	displayName string
	noAttach    bool
	// output selects --no-attach's reply shape: "text" (default, stderr
	// job_id= line) or "json" (stdout {"kind":"session","job_id":"..."}).
	output string
}

func addAgentSessionFlags(cmd *cobra.Command, f *agentSessionFlags) {
	cmd.Flags().StringVarP(&f.projectRef, "project", "p", "", "project ref (id, name, or unique prefix); required")
	cmd.Flags().StringVar(&f.instruction, "instruction", "", "bootstrap prompt delivered as the first user turn (empty uses the harness default)")
	cmd.Flags().BoolVar(&f.readonly, "readonly", false, "mount the project workspace read-only (default: writable)")
	cmd.Flags().StringVar(&f.model, "model", "", "override the harness binary's default model")
	cmd.Flags().StringVar(&f.displayName, "name", "", "human-readable session label (default: \"<harness> session\")")
	cmd.Flags().BoolVar(&f.noAttach, "no-attach", false, "print the job id and exit instead of attaching to the PTY")
	// StringVarP with shorthand "o" locally overrides the root persistent
	// -o/--output flag (cmd/root.go), matching the same override pattern
	// workspaceExportCmd uses for its own differently-scoped --output.
	cmd.Flags().StringVarP(&f.output, "output", "o", "text", "with --no-attach, the reply shape: \"text\" (stderr job_id= line) or \"json\" (stdout {\"kind\":\"session\",\"job_id\":\"...\"})")
	_ = cmd.RegisterFlagCompletionFunc("project", completeProjectRefs)
}

// agentStartOutput is `boid agent <harness> --no-attach --output json`'s
// stdout shape. "kind" is always "session" here — this CLI only ever starts
// a session.
type agentStartOutput struct {
	Kind  string `json:"kind"`
	JobID string `json:"job_id"`
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
			Args:        cobra.NoArgs,
			Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runAgentSession(cmd.Context(), h, flags)
			},
		}
		addAgentSessionFlags(c, flags)
		agentCmd.AddCommand(c)
	}
}

func runAgentSession(ctx context.Context, harness string, flags *agentSessionFlags) error {
	if flags.projectRef == "" {
		return errors.New("--project is required")
	}
	output := flags.output
	if output == "" {
		output = "text"
	}
	if output != "text" && output != "json" {
		return fmt.Errorf("--output must be \"text\" or \"json\" (got %q)", flags.output)
	}
	if output == "json" && !flags.noAttach {
		return errors.New("--output json requires --no-attach")
	}
	c := client.FromContext(ctx)

	// Resolve the project ref to its id so the URL path is canonical
	// (matches how /api/projects/{id}/sessions is mounted).
	project, err := resolveProjectRef(c, os.Stdin, os.Stderr, flags.projectRef)
	if err != nil {
		return fmt.Errorf("resolve project ref %q: %w", flags.projectRef, err)
	}
	projectID := project.ID

	req := apiwire.StartSessionRequest{
		ProjectID:   projectID,
		HarnessType: harness,
		Instruction: flags.instruction,
		Readonly:    flags.readonly,
		Model:       flags.model,
		DisplayName: flags.displayName,
	}
	var result apiwire.StartSessionResult
	if err := c.Do("POST", fmt.Sprintf("/api/projects/%s/sessions", projectID), req, &result); err != nil {
		return fmt.Errorf("start %s session: %w", harness, err)
	}

	// Attach mode hands the terminal straight to the harness, so a leading
	// `job_id=...` line just clutters the agent's startup output. Print it
	// only when the caller asked us not to attach (script use, daemon job
	// inspection, etc.) where the id is the only useful output.
	if flags.noAttach {
		if output == "json" {
			return json.NewEncoder(os.Stdout).Encode(agentStartOutput{Kind: "session", JobID: result.JobID})
		}
		fmt.Fprintf(os.Stderr, "job_id=%s\n", result.JobID)
		return nil
	}
	return attachToJob(ctx, result.JobID)
}

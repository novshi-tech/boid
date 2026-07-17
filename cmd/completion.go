package cmd

import (
	"context"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

const (
	completionSocketProbeTimeout = 200 * time.Millisecond
	// completionAPIRequestTimeout caps the full GET /api/projects round
	// trip during shell TAB completion. The probe above answers "is
	// anything listening" cheaply; this deadline covers the "is it
	// answering FAST" case — a daemon that accepts the TCP connection
	// then hangs must not block the user's shell.
	completionAPIRequestTimeout = 2 * time.Second
)

// isCompletionQuery reports whether cmd is an actual TAB-completion query
// (Cobra's hidden `__complete` / `__completeNoDesc` commands, invoked by
// the shell every time the user hits TAB). These run against a specific
// target command whose --profile flag may or may not have been parsed
// yet, so root's PersistentPreRunE treats a resolution failure here as
// "silently degrade to no candidates" rather than a hard error the
// user's shell would surface.
func isCompletionQuery(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "__complete", "__completeNoDesc":
			return true
		}
	}
	return false
}

// isCompletionScriptGen reports whether cmd is the `boid completion
// bash|zsh|fish|powershell` script-generation entrypoint (as opposed to a
// live TAB query). Script generation is genuinely neutral: no daemon,
// no profile, no token needed — a missing or broken profile file must
// NOT prevent the user from re-installing their shell completion.
func isCompletionScriptGen(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "completion" {
			return true
		}
	}
	return false
}

// completeProjectRefs supplies project ids and names as completion candidates.
// It returns nothing when the daemon is unreachable so TAB never blocks.
//
// Uses FromContextOrNil (NOT FromContext): a completion query with a
// broken profile file reaches this function with no client injected,
// because root's PersistentPreRunE swallowed the resolve error to keep
// the shell quiet. If we then fell back to the default UNIX client the
// user would silently see candidates from whichever daemon happens to
// be listening on the local socket — the WRONG daemon for their
// selected profile. Returning "no candidates" is the correct degrade.
//
// The liveness probe uses the profile-resolved client (client.ProbeAlive)
// rather than a hard-coded default-UNIX-socket dial, so shell completion
// works uniformly across unix and https profiles — a non-default unix
// profile finds its own socket, and an https profile does a bounded TCP
// connect against its own origin.
func completeProjectRefs(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	c := client.FromContextOrNil(cmd.Context())
	if c == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if !c.ProbeAlive(completionSocketProbeTimeout) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// A daemon that accepts our connection then never answers must NOT
	// hang the user's shell — bound the whole request with a wall-clock
	// timeout.
	ctx, cancel := context.WithTimeout(cmd.Context(), completionAPIRequestTimeout)
	defer cancel()
	var projects []projectspec.Project
	if err := c.DoContext(ctx, "GET", "/api/projects", nil, &projects); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := make([]string, 0, len(projects)*2)
	for _, p := range projects {
		if p.Meta.Name != "" {
			out = append(out, p.Meta.Name)
		}
		if p.ID != "" {
			out = append(out, p.ID)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}


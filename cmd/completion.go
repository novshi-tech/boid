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
	// completionAPIRequestTimeout bounds the full API round trip so a
	// daemon that accepts the connection then hangs can't block the shell.
	completionAPIRequestTimeout = 2 * time.Second
)

// isCompletionQuery reports whether cmd is an actual TAB-completion query
// (Cobra's hidden `__complete` / `__completeNoDesc` commands). A resolution
// failure here must degrade to no candidates, never a hard error.
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
// bash|zsh|fish|powershell` script-generation entrypoint, which needs no
// daemon or profile and must work even with a broken profile file.
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
// Uses FromContextOrNil, not FromContext: with no client injected (broken
// profile file), falling back to the default UNIX client would silently
// query the wrong daemon, so "no candidates" is the correct degrade.
func completeProjectRefs(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	c := client.FromContextOrNil(cmd.Context())
	if c == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if !c.ProbeAlive(completionSocketProbeTimeout) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
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

package cmd

import (
	"net"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

const completionSocketProbeTimeout = 200 * time.Millisecond

func isCompletionInvocation(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "__complete", "__completeNoDesc", "completion":
			return true
		}
	}
	return false
}

func daemonReady() bool {
	conn, err := net.DialTimeout("unix", client.DefaultSocketPath(), completionSocketProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// completeProjectRefs supplies project ids and names as completion candidates.
// It returns nothing when the daemon is unreachable so TAB never blocks.
func completeProjectRefs(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	if !daemonReady() {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c := client.NewUnixClient(client.DefaultSocketPath())
	var projects []projectspec.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
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

// completeExecCommandNames supplies command names defined for the project
// referenced by the -p flag. Returns nothing when no -p was given yet, the
// daemon is down, or the ref does not resolve.
func completeExecCommandNames(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveDefault
	}
	if execProjectRef == "" || !daemonReady() {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c := client.NewUnixClient(client.DefaultSocketPath())
	var p projectspec.Project
	if err := c.Do("GET", "/api/projects/"+execProjectRef, nil, &p); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var resp struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := c.Do("GET", "/api/projects/"+p.ID+"/commands", nil, &resp); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := make([]string, 0, len(resp.Commands))
	for _, cmd := range resp.Commands {
		out = append(out, cmd.Name)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

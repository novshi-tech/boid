package cmd

import (
	"fmt"
	"os"
	"syscall"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var execCmd = &cobra.Command{
	Use:           "exec -p <ref> -- <argv...>",
	Short:         "Run an arbitrary command inside a project sandbox",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.MinimumNArgs(1),
	RunE:          runExec,
}

var (
	execProjectRef string
	execName       string
	execReadonly   bool
)

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execProjectRef, "project", "p", "", "project ref (id or name, partial match supported)")
	execCmd.Flags().StringVar(&execName, "name", "", "session display name (defaults to argv[0])")
	execCmd.Flags().BoolVar(&execReadonly, "readonly", false, "mount the project workspace read-only")
	execCmd.Flags().SetInterspersed(false)
	_ = execCmd.RegisterFlagCompletionFunc("project", completeProjectRefs)
}

func runExec(cobraCmd *cobra.Command, args []string) error {
	if execProjectRef == "" {
		return fmt.Errorf("-p/--project is required")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	p, err := resolveProjectRef(c, os.Stdin, os.Stdout, execProjectRef)
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	// Re-fetch with meta hydrated so we inherit project-level traits
	// (host_commands / env / additional_bindings / kit_roots / capabilities).
	// The server-side handler hydrates project.Meta against the linked
	// workspace.yaml (Capabilities / Env / SecretNamespace) before
	// returning, so the SessionJobInput below sees the merged view.
	var project orchestrator.Project
	if err := c.Do("GET", "/api/projects/"+p.ID, nil, &project); err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	var workspacePeers map[string]string
	if project.WorkspaceID != "" {
		var peers []orchestrator.Project
		if err := c.Do("GET", "/api/projects?workspace_id="+project.WorkspaceID, nil, &peers); err == nil {
			workspacePeers = make(map[string]string)
			for _, peer := range peers {
				if peer.ID != project.ID {
					workspacePeers[peer.ID] = peer.WorkDir
				}
			}
			if len(workspacePeers) == 0 {
				workspacePeers = nil
			}
		}
	}

	var proxyInfo struct{ Port int }
	_ = c.Do("GET", "/api/proxy", nil, &proxyInfo)

	interactive := false
	if fileInfo, _ := os.Stdin.Stat(); fileInfo.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}

	spec, err := dispatcher.BuildExecJobSpec(dispatcher.SessionJobInput{
		ProjectID:          project.ID,
		ProjectWorkDir:     project.WorkDir,
		Readonly:           execReadonly,
		Env:                project.Meta.Env,
		HostCommands:       map[string]orchestrator.HostCommandSpec(project.Meta.HostCommands),
		AdditionalBindings: project.Meta.AdditionalBindings,
		SecretNamespace:    project.Meta.SecretNamespace,
		DockerEnabled:      project.Meta.Capabilities.Docker != nil,
		DisplayName:        execName,
	}, args, interactive)
	if err != nil {
		return fmt.Errorf("build exec job spec: %w", err)
	}

	var brokerSocket, brokerToken string
	var resolvedHostCommands map[string]orchestrator.CommandDef
	if len(spec.HostCommands) > 0 || len(spec.BuiltinPolicies) > 0 {
		var brokerResp struct {
			Token                string                             `json:"token"`
			Socket               string                             `json:"socket"`
			ResolvedHostCommands map[string]orchestrator.CommandDef `json:"resolved_host_commands,omitempty"`
		}
		regReq := map[string]any{
			"commands":         map[string]orchestrator.HostCommandSpec(project.Meta.HostCommands),
			"builtin_policies": dispatcher.PoliciesToSandbox(spec.BuiltinPolicies),
			"project_id":       project.ID,
		}
		if err := c.Do("POST", "/api/broker/register", regReq, &brokerResp); err == nil {
			brokerSocket = brokerResp.Socket
			brokerToken = brokerResp.Token
			resolvedHostCommands = brokerResp.ResolvedHostCommands
		}
	}

	boidBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve boid binary: %w", err)
	}
	rt := dispatcher.SandboxRuntimeInfo{
		JobID:                fmt.Sprintf("exec-%s", project.ID),
		BoidBinary:           boidBinary,
		ServerSocket:         client.DefaultSocketPath(),
		ProxyPort:            proxyInfo.Port,
		BrokerSocket:         brokerSocket,
		BrokerToken:          brokerToken,
		Foreground:           true,
		WorkspacePeers:       workspacePeers,
		ResolvedHostCommands: resolvedHostCommands,
	}

	sbSpec, err := dispatcher.BuildSandboxSpec(spec, rt)
	if err != nil {
		return fmt.Errorf("build sandbox spec: %w", err)
	}

	sb, err := dispatcher.NewSandboxPreparer().PrepareSandbox(sbSpec)
	if err != nil {
		return fmt.Errorf("prepare sandbox: %w", err)
	}
	if sb == nil || sb.SpecPath == "" {
		return fmt.Errorf("prepare sandbox: missing spec path")
	}

	// Exec the go-native sandbox launcher in place of this process. boid exec is
	// foreground (Foreground=true → no broker job-done), so runner-outer just
	// runs the command in the sandbox and propagates its exit code.
	runnerArgs := []string{boidBinary, "runner-outer", "--spec", sb.SpecPath, "--state", sb.StatePath}
	return syscall.Exec(boidBinary, runnerArgs, os.Environ())
}

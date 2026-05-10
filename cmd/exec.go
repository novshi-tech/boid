package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var execCmd = &cobra.Command{
	Use:           "exec -p <ref> <command-name> [args...]",
	Short:         "Execute a named command in a project sandbox",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.ArbitraryArgs,
	RunE:          runExec,
}

var execProjectRef string

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execProjectRef, "project", "p", "", "project ref (id or name, partial match supported)")
	execCmd.Flags().SetInterspersed(false)
}

type execProjectData struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	WorkDir     string `json:"work_dir"`
}

// execCommandResponse mirrors the /api/projects/<id>/commands/<name> JSON
// payload. Field types stay wire-compatible with the daemon's responder.
type execCommandResponse struct {
	Command            []string                                `json:"command"`
	Env                map[string]string                       `json:"env,omitempty"`
	HostCommands       map[string]orchestrator.HostCommandSpec `json:"host_commands,omitempty"`
	AdditionalBindings []orchestrator.BindMount                `json:"additional_bindings,omitempty"`
	Readonly           bool                                    `json:"readonly,omitempty"`
}

type execPreparedJob struct {
	spec    *orchestrator.JobSpec
	rt      dispatcher.SandboxRuntimeInfo
	tty     bool
}

func buildExecJob(projectID, commandName string, userArgs []string) (*execPreparedJob, error) {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var p execProjectData
	if err := c.Do("GET", "/api/projects/"+projectID, nil, &p); err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}

	var cmd execCommandResponse
	if err := c.Do("GET", "/api/projects/"+projectID+"/commands/"+commandName, nil, &cmd); err != nil {
		return nil, fmt.Errorf("get command %q: %w", commandName, err)
	}

	boidBinary, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve boid binary: %w", err)
	}

	var workspacePeers map[string]string
	if p.WorkspaceID != "" {
		var peers []execProjectData
		if err := c.Do("GET", "/api/projects?workspace_id="+p.WorkspaceID, nil, &peers); err == nil {
			workspacePeers = make(map[string]string)
			for _, peer := range peers {
				if peer.ID != projectID {
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

	spec := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:          p.ID,
		ProjectWorkDir:     p.WorkDir,
		Argv:               append(cmd.Command, userArgs...),
		Env:                cmd.Env,
		HostCommands:       cmd.HostCommands,
		AdditionalBindings: cmd.AdditionalBindings,
		Readonly:           cmd.Readonly,
		// Interactive=false: TTY is overridden in runExec based on real terminal state.
	})

	var brokerSocket, brokerToken string
	var resolvedHostCommands map[string]orchestrator.CommandDef
	if len(spec.HostCommands) > 0 || len(spec.BuiltinPolicies) > 0 {
		var brokerResp struct {
			Token                string                                `json:"token"`
			Socket               string                                `json:"socket"`
			ResolvedHostCommands map[string]orchestrator.CommandDef    `json:"resolved_host_commands,omitempty"`
		}
		regReq := map[string]any{
			"commands":         cmd.HostCommands,
			"builtin_policies": dispatcher.PoliciesToSandbox(spec.BuiltinPolicies),
			"project_id":       p.ID,
		}
		if err := c.Do("POST", "/api/broker/register", regReq, &brokerResp); err == nil {
			brokerSocket = brokerResp.Socket
			brokerToken = brokerResp.Token
			resolvedHostCommands = brokerResp.ResolvedHostCommands
		}
	}

	rt := dispatcher.SandboxRuntimeInfo{
		JobID:                fmt.Sprintf("exec-%s", projectID),
		BoidBinary:           boidBinary,
		ServerSocket:         client.DefaultSocketPath(),
		ProxyPort:            proxyInfo.Port,
		BrokerSocket:         brokerSocket,
		BrokerToken:          brokerToken,
		Foreground:           true,
		WorkspacePeers:       workspacePeers,
		ResolvedHostCommands: resolvedHostCommands,
	}
	return &execPreparedJob{spec: spec, rt: rt}, nil
}

func listExecCommands(projectID string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var projectInfo execProjectData
	if err := c.Do("GET", "/api/projects/"+projectID, nil, &projectInfo); err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	var resp struct {
		Commands []struct {
			Name    string   `json:"name"`
			Command []string `json:"command"`
		} `json:"commands"`
	}
	if err := c.Do("GET", "/api/projects/"+projectID+"/commands", nil, &resp); err != nil {
		return fmt.Errorf("list commands: %w", err)
	}

	if len(resp.Commands) == 0 {
		fmt.Printf("No commands defined for project %s.\n", projectID)
		fmt.Printf("Run 'boid exec -p <ref> <command>' to execute.\n")
		return nil
	}

	maxLen := 0
	for _, cmd := range resp.Commands {
		if len(cmd.Name) > maxLen {
			maxLen = len(cmd.Name)
		}
	}
	fmt.Printf("Available commands for project %s:\n", projectID)
	for _, cmd := range resp.Commands {
		cmdStr := ""
		if len(cmd.Command) > 0 {
			cmdStr = cmd.Command[0]
			if len(cmd.Command) > 1 {
				cmdStr += " " + cmd.Command[1]
			}
		}
		fmt.Printf("  %-*s  %s\n", maxLen, cmd.Name, cmdStr)
	}
	fmt.Printf("\nRun 'boid exec -p <ref> <command> [args...]' to execute.\n")
	return nil
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

	if len(args) == 0 {
		return listExecCommands(p.ID)
	}

	commandName := args[0]
	userArgs := args[1:]
	prepared, err := buildExecJob(p.ID, commandName, userArgs)
	if err != nil {
		return err
	}

	if fileInfo, _ := os.Stdin.Stat(); fileInfo.Mode()&os.ModeCharDevice != 0 {
		prepared.tty = true
	}

	sbSpec, err := dispatcher.BuildSandboxSpec(prepared.spec, prepared.rt)
	if err != nil {
		return fmt.Errorf("build sandbox spec: %w", err)
	}
	// exec is interactive / terminal-driven; override TTY only when caller has a real TTY.
	sbSpec.TTY = prepared.tty

	outerPath, err := dispatcher.NewSandboxPreparer().PrepareSandbox(sbSpec)
	if err != nil {
		return fmt.Errorf("prepare sandbox: %w", err)
	}
	if outerPath == nil || outerPath.OuterPath == "" {
		return fmt.Errorf("prepare sandbox: missing outer script path")
	}

	bashArgs := []string{"bash", outerPath.OuterPath}
	if os.Getenv("BOID_DEBUG") != "" {
		bashArgs = []string{"bash", "-x", outerPath.OuterPath}
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("find bash: %w", err)
	}

	return syscall.Exec(bashPath, bashArgs, os.Environ())
}

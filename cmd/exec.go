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
	Use:           "exec -p <ref> [command-name]",
	Short:         "Execute a named command in a project sandbox",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.MaximumNArgs(1),
	RunE:          runExec,
}

var execProjectRef string

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execProjectRef, "project", "p", "", "project ref (id or name, partial match supported)")
}

type execProjectData struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	WorkDir     string `json:"work_dir"`
}

type execCommandResponse struct {
	Command            []string                             `json:"command"`
	Env                map[string]string                    `json:"env,omitempty"`
	BuiltinCommands    []string                             `json:"builtin_commands,omitempty"`
	HostCommands       map[string]dispatcher.ExecCommandDef `json:"host_commands,omitempty"`
	AdditionalBindings []dispatcher.ExecBindMount           `json:"additional_bindings,omitempty"`
}

// buildExecRequest creates a dispatcher exec request from the resolved command for a project.
func buildExecRequest(projectID, commandName string) (dispatcher.ExecRequest, error) {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var p execProjectData
	if err := c.Do("GET", "/api/projects/"+projectID, nil, &p); err != nil {
		return dispatcher.ExecRequest{}, fmt.Errorf("get project: %w", err)
	}

	var cmd execCommandResponse
	if err := c.Do("GET", "/api/projects/"+projectID+"/commands/"+commandName, nil, &cmd); err != nil {
		return dispatcher.ExecRequest{}, fmt.Errorf("get command %q: %w", commandName, err)
	}

	boidBinary, err := os.Executable()
	if err != nil {
		return dispatcher.ExecRequest{}, fmt.Errorf("resolve boid binary: %w", err)
	}

	homeDir, _ := os.UserHomeDir()

	// Collect workspace peer projects (read-only mounts)
	var workspaceDirs map[string]string
	if p.WorkspaceID != "" {
		var peers []execProjectData
		if err := c.Do("GET", "/api/projects?workspace_id="+p.WorkspaceID, nil, &peers); err == nil {
			workspaceDirs = make(map[string]string)
			for _, peer := range peers {
				if peer.ID != projectID {
					workspaceDirs[peer.ID] = peer.WorkDir
				}
			}
			if len(workspaceDirs) == 0 {
				workspaceDirs = nil
			}
		}
	}

	// Get proxy port from server
	var proxyInfo struct{ Port int }
	c.Do("GET", "/api/proxy", nil, &proxyInfo) //nolint:errcheck

	// Build builtin commands list (always include boid)
	builtinCommands := append([]string(nil), cmd.BuiltinCommands...)
	hasBoid := false
	for _, bc := range builtinCommands {
		if bc == "boid" {
			hasBoid = true
			break
		}
	}
	if !hasBoid {
		builtinCommands = append(builtinCommands, "boid")
	}
	builtinPolicies := orchestrator.DefaultBuiltinPolicies(orchestrator.RoleGate, builtinCommands, orchestrator.PolicyContext{ProjectDir: p.WorkDir})

	var brokerSocket, brokerToken string
	if len(cmd.HostCommands) > 0 || len(builtinPolicies) > 0 {
		var brokerResp struct {
			Token  string `json:"token"`
			Socket string `json:"socket"`
		}
		regReq := map[string]any{
			"commands":         cmd.HostCommands,
			"builtin_policies": dispatcher.PoliciesToSandbox(builtinPolicies),
			"project_id":       p.ID,
		}
		if err := c.Do("POST", "/api/broker/register", regReq, &brokerResp); err == nil {
			brokerSocket = brokerResp.Socket
			brokerToken = brokerResp.Token
		}
	}

	environmentYAML := orchestrator.BuildEnvironmentYAML(false, false, true, workspaceDirs, cmd.BuiltinCommands)

	req := dispatcher.ExecRequest{
		JobID:              fmt.Sprintf("exec-%s", projectID),
		ProjectID:          p.ID,
		ProjectDir:         p.WorkDir,
		HomeDir:            homeDir,
		Argv:               cmd.Command,
		BoidBinary:         boidBinary,
		ServerSocket:       client.DefaultSocketPath(),
		BrokerSocket:       brokerSocket,
		BrokerToken:        brokerToken,
		Env:                cmd.Env,
		BuiltinPolicies:    builtinPolicies,
		HostCommands:       cmd.HostCommands,
		AdditionalBindings: cmd.AdditionalBindings,
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          proxyInfo.Port,
		EnvironmentYAML:    environmentYAML,
	}

	return req, nil
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
	fmt.Printf("\nRun 'boid exec -p <ref> <command>' to execute.\n")
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
	req, err := buildExecRequest(p.ID, commandName)
	if err != nil {
		return err
	}

	if fileInfo, _ := os.Stdin.Stat(); fileInfo.Mode()&os.ModeCharDevice != 0 {
		req.TTY = true
	}

	outerPath, err := dispatcher.WriteExecScripts(req, dispatcher.NewSandboxPreparer())
	if err != nil {
		return fmt.Errorf("write sandbox scripts: %w", err)
	}

	bashArgs := []string{"bash", outerPath}
	if os.Getenv("BOID_DEBUG") != "" {
		bashArgs = []string{"bash", "-x", outerPath}
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("find bash: %w", err)
	}

	return syscall.Exec(bashPath, bashArgs, os.Environ())
}

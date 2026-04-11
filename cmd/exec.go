package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var execCmd = &cobra.Command{
	Use:           "exec <project-id> -- <command...>",
	Short:         "Execute a command in a project sandbox",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.MinimumNArgs(1),
	RunE:          runExec,
}

func init() {
	rootCmd.AddCommand(execCmd)
}

type execProjectMeta struct {
	BuiltinCommands    []string                             `json:"builtin_commands"`
	HostCommands       map[string]dispatcher.ExecCommandDef `json:"host_commands"`
	AdditionalBindings []dispatcher.ExecBindMount           `json:"additional_bindings"`
	Env                map[string]string                    `json:"env"`
}

type execProject struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	WorkDir     string          `json:"work_dir"`
	Meta        execProjectMeta `json:"meta"`
}

// buildExecRequest creates a dispatcher exec request from the server state for a project.
func buildExecRequest(projectID string) (dispatcher.ExecRequest, error) {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var p execProject
	if err := c.Do("GET", "/api/projects/"+projectID, nil, &p); err != nil {
		return dispatcher.ExecRequest{}, fmt.Errorf("get project: %w", err)
	}

	boidBinary, err := os.Executable()
	if err != nil {
		return dispatcher.ExecRequest{}, fmt.Errorf("resolve boid binary: %w", err)
	}

	homeDir, _ := os.UserHomeDir()

	// Collect workspace peer projects (read-only mounts)
	var workspaceDirs map[string]string
	if p.WorkspaceID != "" {
		var peers []execProject
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
	c.Do("GET", "/api/proxy", nil, &proxyInfo)

	// Register host commands with broker
	builtinPolicies := orchestrator.DefaultBuiltinPolicies(orchestrator.RoleGate, p.Meta.BuiltinCommands)
	var brokerSocket, brokerToken string
	if len(p.Meta.HostCommands) > 0 || len(builtinPolicies) > 0 {
		var brokerResp struct {
			Token  string `json:"token"`
			Socket string `json:"socket"`
		}
		regReq := map[string]any{
			"commands":         p.Meta.HostCommands,
			"builtin_policies": builtinPolicies,
			"project_id":       p.ID,
		}
		if err := c.Do("POST", "/api/broker/register", regReq, &brokerResp); err == nil {
			brokerSocket = brokerResp.Socket
			brokerToken = brokerResp.Token
		}
	}

	req := dispatcher.ExecRequest{
		JobID:              fmt.Sprintf("exec-%s", projectID),
		ProjectID:          p.ID,
		ProjectDir:         p.WorkDir,
		HomeDir:            homeDir,
		BoidBinary:         boidBinary,
		ServerSocket:       client.DefaultSocketPath(),
		BrokerSocket:       brokerSocket,
		BrokerToken:        brokerToken,
		Env:                p.Meta.Env,
		BuiltinPolicies:    builtinPolicies,
		HostCommands:       p.Meta.HostCommands,
		AdditionalBindings: p.Meta.AdditionalBindings,
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          proxyInfo.Port,
	}

	return req, nil
}

func runExec(cmd *cobra.Command, args []string) error {
	projectID := args[0]

	// Parse command after "--" from os.Args
	var command string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			command = strings.Join(os.Args[i+1:], " ")
			break
		}
	}
	if command == "" {
		return fmt.Errorf("usage: boid exec <project-id> -- <command...>")
	}

	req, err := buildExecRequest(projectID)
	if err != nil {
		return err
	}
	req.Command = command
	if fileInfo, _ := os.Stdin.Stat(); fileInfo.Mode()&os.ModeCharDevice != 0 {
		req.TTY = true
	}

	outerPath, err := dispatcher.WriteExecScripts(req, dispatcher.NewSandboxPreparer())
	if err != nil {
		return fmt.Errorf("write sandbox scripts: %w", err)
	}

	// Run sandbox in foreground with stdin/stdout/stderr passthrough
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

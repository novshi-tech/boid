package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/sandbox"
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

// buildSandboxConfig creates a WrapperConfig from the server state for a given project.
func buildSandboxConfig(projectID string) (sandbox.WrapperConfig, error) {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var p project.Project
	if err := c.Do("GET", "/api/projects/"+projectID, nil, &p); err != nil {
		return sandbox.WrapperConfig{}, fmt.Errorf("get project: %w", err)
	}

	boidBinary, err := os.Executable()
	if err != nil {
		return sandbox.WrapperConfig{}, fmt.Errorf("resolve boid binary: %w", err)
	}

	homeDir, _ := os.UserHomeDir()

	// Collect workspace peer projects (read-only mounts)
	var workspaceDirs map[string]string
	if p.Meta.WorkspaceID != "" {
		var peers []project.Project
		if err := c.Do("GET", "/api/projects?workspace_id="+p.Meta.WorkspaceID, nil, &peers); err == nil {
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
	var brokerSocket, brokerToken string
	var hostCommandNames []string
	if len(p.Meta.HostCommands) > 0 {
		var brokerResp struct {
			Token  string `json:"token"`
			Socket string `json:"socket"`
		}
		regReq := map[string]any{
			"commands": p.Meta.HostCommands,
		}
		if err := c.Do("POST", "/api/broker/register", regReq, &brokerResp); err == nil {
			brokerSocket = brokerResp.Socket
			brokerToken = brokerResp.Token
		}
		for name := range p.Meta.HostCommands {
			hostCommandNames = append(hostCommandNames, name)
		}
	}

	cfg := sandbox.WrapperConfig{
		JobID:              fmt.Sprintf("exec-%s", projectID),
		ProjectID:          p.Meta.ID,
		ProjectDir:         p.WorkDir,
		HomeDir:            homeDir,
		BoidBinary:         boidBinary,
		ServerSocket:       client.DefaultSocketPath(),
		BrokerSocket:       brokerSocket,
		BrokerToken:        brokerToken,
		Env:                p.Meta.Env,
		HostCommands:       hostCommandNames,
		AdditionalBindings: toSandboxBindings(p.Meta.AdditionalBindings),
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          proxyInfo.Port,
	}

	return cfg, nil
}

func toSandboxBindings(bindings []project.BindMount) []sandbox.BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]sandbox.BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, sandbox.BindMount{
			Source: binding.Source,
			Mode:   binding.Mode,
		})
	}
	return out
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

	cfg, err := buildSandboxConfig(projectID)
	if err != nil {
		return err
	}
	cfg.Command = command
	if fileInfo, _ := os.Stdin.Stat(); fileInfo.Mode()&os.ModeCharDevice != 0 {
		cfg.TTY = true
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

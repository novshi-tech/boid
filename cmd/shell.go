package cmd

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/job"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/tmux"
	"github.com/spf13/cobra"
)

var shellCmd = &cobra.Command{
	Use:   "shell <project-id>",
	Short: "Open a sandbox shell for a project",
	Args:  cobra.ExactArgs(1),
	RunE:  runShell,
}

func init() {
	rootCmd.AddCommand(shellCmd)
}

func runShell(cmd *cobra.Command, args []string) error {
	projectID := args[0]

	c := client.NewUnixClient(client.DefaultSocketPath())
	var p model.Project
	if err := c.Do("GET", "/api/projects/"+projectID, nil, &p); err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	boidBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve boid binary: %w", err)
	}

	// Collect workspace peer projects (read-only mounts)
	var workspaceDirs map[string]string
	if p.Meta.WorkspaceID != "" {
		var peers []model.Project
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

	cfg := job.WrapperConfig{
		JobID:              fmt.Sprintf("shell-%s", projectID),
		ProjectID:          p.Meta.ID,
		ProjectDir:         p.WorkDir,
		BoidBinary:         boidBinary,
		ServerSocket:       client.DefaultSocketPath(),
		BrokerSocket:       brokerSocket,
		BrokerToken:        brokerToken,
		Env:                p.Meta.Env,
		HostCommands:       hostCommandNames,
		AdditionalBindings: p.Meta.AdditionalBindings,
		WorkspaceDirs:      workspaceDirs,
		ProxyPort:          proxyInfo.Port,
		Interactive:        true,
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		return fmt.Errorf("write sandbox scripts: %w", err)
	}

	t := &tmux.RealTmux{}
	session := "boid"
	windowName := fmt.Sprintf("shell-%s", projectID)

	if err := t.EnsureSession(session); err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}
	if err := t.RunInWindow(session, windowName, fmt.Sprintf("bash %s", outerPath)); err != nil {
		return fmt.Errorf("run in window: %w", err)
	}

	// If already inside tmux, switch to the new window; otherwise attach.
	if os.Getenv("TMUX") != "" {
		return t.SwitchClient(session, windowName)
	}
	return t.Attach(session)
}

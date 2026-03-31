package dispatcher

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/sandbox"
)

type ExecBindMount struct {
	Source string `json:"source"`
	Mode   string `json:"mode"`
}

type ExecCommandDef struct {
	Name                string            `json:"name"`
	Path                string            `json:"path"`
	AllowedPatterns     []string          `json:"allowed_patterns"`
	DeniedPatterns      []string          `json:"denied_patterns"`
	AllowedSubcommands  []string          `json:"allowed_subcommands"`
	AllowStdin          bool              `json:"allow_stdin"`
	Env                 map[string]string `json:"env"`
	ExtractSubcommandFn string            `json:"extract_subcommand_fn"`
	RequireCwd          bool              `json:"require_cwd"`
	AllowedCwdPrefixes  []string          `json:"allowed_cwd_prefixes"`
}

type ExecRequest struct {
	JobID              string
	ProjectID          string
	ProjectDir         string
	HomeDir            string
	Command            string
	BoidBinary         string
	ServerSocket       string
	BrokerSocket       string
	BrokerToken        string
	Env                map[string]string
	HostCommands       map[string]ExecCommandDef
	AdditionalBindings []ExecBindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	TTY                bool
}

func WriteExecScripts(req ExecRequest) (string, error) {
	cfg, err := buildExecWrapperConfig(req)
	if err != nil {
		return "", err
	}
	return sandbox.WriteSandboxScripts(cfg)
}

func buildExecWrapperConfig(req ExecRequest) (sandbox.WrapperConfig, error) {
	if req.JobID == "" {
		return sandbox.WrapperConfig{}, fmt.Errorf("job id is required")
	}
	if req.ProjectID == "" {
		return sandbox.WrapperConfig{}, fmt.Errorf("project id is required")
	}
	if req.ProjectDir == "" {
		return sandbox.WrapperConfig{}, fmt.Errorf("project dir is required")
	}
	if req.Command == "" {
		return sandbox.WrapperConfig{}, fmt.Errorf("command is required")
	}
	if req.BoidBinary == "" {
		return sandbox.WrapperConfig{}, fmt.Errorf("boid binary is required")
	}

	return sandbox.WrapperConfig{
		JobID:              req.JobID,
		ProjectID:          req.ProjectID,
		ProjectDir:         req.ProjectDir,
		HomeDir:            req.HomeDir,
		Command:            req.Command,
		BoidBinary:         req.BoidBinary,
		ServerSocket:       req.ServerSocket,
		BrokerSocket:       req.BrokerSocket,
		BrokerToken:        req.BrokerToken,
		Env:                req.Env,
		HostCommands:       execHostCommandNames(req.HostCommands),
		AdditionalBindings: execBindMounts(req.AdditionalBindings),
		WorkspaceDirs:      req.WorkspaceDirs,
		ProxyPort:          req.ProxyPort,
		TTY:                req.TTY,
	}, nil
}

func execHostCommandNames(cmds map[string]ExecCommandDef) []string {
	if len(cmds) == 0 {
		return nil
	}
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	return names
}

func execBindMounts(bindings []ExecBindMount) []sandbox.BindMount {
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

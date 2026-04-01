package dispatcher

import (
	"fmt"
)

type ExecBindMount struct {
	Source string `json:"source"`
	Mode   string `json:"mode"`
}

type ExecCommandDef struct {
	Allow []string          `json:"allow,omitempty"`
	Deny  []string          `json:"deny,omitempty"`
	Stdin bool              `json:"stdin,omitempty"`
	Path  string            `json:"path,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
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
	BuiltinCommands    []string
	HostCommands       map[string]ExecCommandDef
	AdditionalBindings []ExecBindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	TTY                bool
}

func WriteExecScripts(req ExecRequest, preparer SandboxPreparer) (string, error) {
	spec, err := buildExecSandboxSpec(req)
	if err != nil {
		return "", err
	}
	if preparer == nil {
		return "", fmt.Errorf("sandbox preparer is required")
	}
	prepared, err := preparer.PrepareSandbox(spec)
	if err != nil {
		return "", err
	}
	if prepared == nil || prepared.OuterPath == "" {
		return "", fmt.Errorf("prepare sandbox: missing outer script path")
	}
	return prepared.OuterPath, nil
}

func buildExecSandboxSpec(req ExecRequest) (SandboxSpec, error) {
	if req.JobID == "" {
		return SandboxSpec{}, fmt.Errorf("job id is required")
	}
	if req.ProjectID == "" {
		return SandboxSpec{}, fmt.Errorf("project id is required")
	}
	if req.ProjectDir == "" {
		return SandboxSpec{}, fmt.Errorf("project dir is required")
	}
	if req.Command == "" {
		return SandboxSpec{}, fmt.Errorf("command is required")
	}
	if req.BoidBinary == "" {
		return SandboxSpec{}, fmt.Errorf("boid binary is required")
	}

	return SandboxSpec{
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
		BuiltinCommands:    cloneStrings(req.BuiltinCommands),
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

func execBindMounts(bindings []ExecBindMount) []BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, BindMount{
			Source: binding.Source,
			Mode:   binding.Mode,
		})
	}
	return out
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

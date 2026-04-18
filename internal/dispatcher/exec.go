package dispatcher

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/orchestrator"
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

// ExecRequest carries the fields cmd/exec.go gathers from the API before
// invoking the sandbox. Dispatcher uses orchestrator.BuildExecSandboxSpec
// to translate it into a primitive sandbox.Spec.
type ExecRequest struct {
	JobID              string
	ProjectID          string
	ProjectDir         string
	HomeDir            string
	Argv               []string
	BoidBinary         string
	ServerSocket       string
	BrokerSocket       string
	BrokerToken        string
	Env                map[string]string
	BuiltinPolicies    map[string]orchestrator.BuiltinPolicy
	HostCommands       map[string]ExecCommandDef
	AdditionalBindings []ExecBindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	TTY                bool
	EnvironmentYAML    string
}

// WriteExecScripts materializes the sandbox scripts for a boid exec invocation
// and returns the outer script path. Caller (cmd/exec.go) runs it directly.
func WriteExecScripts(req ExecRequest, preparer SandboxPreparer) (string, error) {
	if req.JobID == "" {
		return "", fmt.Errorf("job id is required")
	}
	if req.ProjectID == "" {
		return "", fmt.Errorf("project id is required")
	}
	if req.ProjectDir == "" {
		return "", fmt.Errorf("project dir is required")
	}
	if len(req.Argv) == 0 {
		return "", fmt.Errorf("argv is required")
	}
	if req.BoidBinary == "" {
		return "", fmt.Errorf("boid binary is required")
	}
	if preparer == nil {
		return "", fmt.Errorf("sandbox preparer is required")
	}

	spec := orchestrator.BuildExecSandboxSpec(orchestrator.ExecSandboxBuildInput{
		JobID:              req.JobID,
		ProjectID:          req.ProjectID,
		ProjectDir:         req.ProjectDir,
		HomeDir:            req.HomeDir,
		Argv:               req.Argv,
		BoidBinary:         req.BoidBinary,
		ServerSocket:       req.ServerSocket,
		BrokerSocket:       req.BrokerSocket,
		BrokerToken:        req.BrokerToken,
		Env:                req.Env,
		BuiltinCommands:    builtinCommandNames(req.BuiltinPolicies),
		HostCommands:       execHostCommandNames(req.HostCommands),
		AdditionalBindings: execToBindMounts(req.AdditionalBindings),
		WorkspaceDirs:      req.WorkspaceDirs,
		ProxyPort:          req.ProxyPort,
		TTY:                req.TTY,
		EnvironmentYAML:    req.EnvironmentYAML,
	})

	prepared, err := preparer.PrepareSandbox(spec)
	if err != nil {
		return "", err
	}
	if prepared == nil || prepared.OuterPath == "" {
		return "", fmt.Errorf("prepare sandbox: missing outer script path")
	}
	return prepared.OuterPath, nil
}

func builtinCommandNames(policies map[string]orchestrator.BuiltinPolicy) []string {
	if len(policies) == 0 {
		return nil
	}
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	return names
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

func execToBindMounts(bindings []ExecBindMount) []orchestrator.BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]orchestrator.BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, orchestrator.BindMount{
			Source: binding.Source,
			Mode:   binding.Mode,
		})
	}
	return out
}

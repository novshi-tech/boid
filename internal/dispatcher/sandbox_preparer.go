package dispatcher

import (
	"sort"

	"github.com/novshi-tech/boid/internal/sandbox"
)

type sandboxPreparerImpl struct{}

// NewSandboxPreparer returns the sandbox provider adapter for dispatcher-owned sandbox specs.
func NewSandboxPreparer() SandboxPreparer {
	return sandboxPreparerImpl{}
}

func (sandboxPreparerImpl) PrepareSandbox(spec SandboxSpec) (*PreparedSandbox, error) {
	outerPath, err := sandbox.WriteSandboxScripts(sandbox.WrapperConfig{
		JobID:              spec.JobID,
		TaskID:             spec.TaskID,
		ProjectID:          spec.ProjectID,
		ProjectDir:         spec.ProjectDir,
		HomeDir:            spec.HomeDir,
		HookFiles:          toSandboxHookFiles(spec.HookFiles),
		GatesDir:           spec.GatesDir,
		HookScript:         spec.HookScript,
		Command:            spec.Command,
		BoidBinary:         spec.BoidBinary,
		ServerSocket:       spec.ServerSocket,
		BrokerSocket:       spec.BrokerSocket,
		BrokerToken:        spec.BrokerToken,
		Env:                spec.Env,
		BuiltinCommands:    sortedPolicyKeys(spec.BuiltinPolicies),
		HostCommands:       spec.HostCommands,
		AdditionalBindings: toSandboxBindMounts(spec.AdditionalBindings),
		WorkspaceDirs:      spec.WorkspaceDirs,
		ProxyPort:          spec.ProxyPort,
		StagingDir:         spec.StagingDir,
		TTY:                spec.TTY,
		Interactive:        spec.Interactive,
		WorktreeDir:        spec.WorktreeDir,
		Role:               spec.Role,
		PayloadJSON:        spec.PayloadJSON,
		TaskJSON:           spec.TaskJSON,
		Readonly:           spec.Readonly,
		InstructionsJSON:   spec.InstructionsJSON,
		TaskYAML:           spec.TaskYAML,
		EnvironmentYAML:    spec.EnvironmentYAML,
		Model:              spec.Model,
		InvokedRole:        spec.InvokedRole,
		InvokedName:        spec.InvokedName,
		InvokedType:        spec.InvokedType,
	})
	if err != nil {
		return nil, err
	}
	return &PreparedSandbox{OuterPath: outerPath}, nil
}

// sortedPolicyKeys extracts the builtin command names from a policy map and
// returns them as a sorted slice for deterministic shim creation.
func sortedPolicyKeys(policies map[string]sandbox.BuiltinPolicy) []string {
	if len(policies) == 0 {
		return nil
	}
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func toSandboxHookFiles(files []HookFile) []sandbox.HookFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]sandbox.HookFile, len(files))
	for i, f := range files {
		out[i] = sandbox.HookFile{Source: f.Source, TargetName: f.TargetName}
	}
	return out
}

func toSandboxBindMounts(bindings []BindMount) []sandbox.BindMount {
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

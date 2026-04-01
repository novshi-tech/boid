package sandbox

import "github.com/novshi-tech/boid/internal/dispatcher"

type dispatcherPreparer struct{}

// NewDispatcherPreparer returns the sandbox provider adapter for dispatcher-owned sandbox specs.
func NewDispatcherPreparer() dispatcher.SandboxPreparer {
	return dispatcherPreparer{}
}

func (dispatcherPreparer) PrepareSandbox(spec dispatcher.SandboxSpec) (*dispatcher.PreparedSandbox, error) {
	outerPath, err := WriteSandboxScripts(WrapperConfig{
		JobID:              spec.JobID,
		TaskID:             spec.TaskID,
		ProjectID:          spec.ProjectID,
		ProjectDir:         spec.ProjectDir,
		HomeDir:            spec.HomeDir,
		HooksDir:           spec.HooksDir,
		HookScript:         spec.HookScript,
		Command:            spec.Command,
		BoidBinary:         spec.BoidBinary,
		ServerSocket:       spec.ServerSocket,
		BrokerSocket:       spec.BrokerSocket,
		BrokerToken:        spec.BrokerToken,
		Env:                spec.Env,
		HostCommands:       spec.HostCommands,
		AdditionalBindings: toSandboxBindMounts(spec.AdditionalBindings),
		WorkspaceDirs:      spec.WorkspaceDirs,
		ProxyPort:          spec.ProxyPort,
		StagingDir:         spec.StagingDir,
		TTY:                spec.TTY,
		WorktreeDir:        spec.WorktreeDir,
		Role:               spec.Role,
		PayloadJSON:        spec.PayloadJSON,
		TaskJSON:           spec.TaskJSON,
		Readonly:           spec.Readonly,
	})
	if err != nil {
		return nil, err
	}
	return &dispatcher.PreparedSandbox{OuterPath: outerPath}, nil
}

func toSandboxBindMounts(bindings []dispatcher.BindMount) []BindMount {
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

package dispatcher

import (
	"context"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type dispatchBackend interface {
	Dispatch(ctx context.Context, plan *DispatchPlan) (string, error)
	WaitForJobCtx(ctx context.Context, jobID string) (JobCompletionResult, error)
}

// OrchestratorAdapter adapts dispatcher execution to orchestrator interfaces.
type OrchestratorAdapter struct {
	dispatcher dispatchBackend
	planner    *orchestrator.DispatchPlanner
}

func NewOrchestratorAdapter(dispatcher dispatchBackend, planner *orchestrator.DispatchPlanner) *OrchestratorAdapter {
	return &OrchestratorAdapter{dispatcher: dispatcher, planner: planner}
}

func (a *OrchestratorAdapter) ExecuteHook(ctx context.Context, event *orchestrator.HookFireEvent) (string, error) {
	request, err := a.planner.PlanHook(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, toDispatchPlan(request))
}

func (a *OrchestratorAdapter) ExecuteGate(ctx context.Context, event *orchestrator.GateFireEvent) (string, error) {
	request, err := a.planner.PlanGate(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, toDispatchPlan(request))
}

func (a *OrchestratorAdapter) WaitForJob(ctx context.Context, jobID string) (orchestrator.JobCompletion, error) {
	result, err := a.dispatcher.WaitForJobCtx(ctx, jobID)
	return orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, err
}

func toDispatchPlan(request *orchestrator.DispatchRequest) *DispatchPlan {
	if request == nil {
		return nil
	}
	return &DispatchPlan{
		TaskID:             request.TaskID,
		ProjectID:          request.ProjectID,
		WorkspaceID:        request.WorkspaceID,
		HandlerID:          request.HandlerID,
		Role:               string(request.Role),
		ProjectDir:         request.ProjectDir,
		HomeDir:            request.HomeDir,
		HookFiles:          toDispatchHookFiles(request.HookFiles),
		GatesDir:           request.GatesDir,
		HookScript:         request.HookScript,
		BoidBinary:         request.BoidBinary,
		ServerSocket:       request.ServerSocket,
		Env:                request.Env,
		BuiltinPolicies:    request.BuiltinPolicies,
		HostCommands:       toCommandDefs(request.HostCommands),
		AdditionalBindings: toBindMounts(request.AdditionalBindings),
		WorkspaceDirs:      request.WorkspaceDirs,
		ProxyPort:          request.ProxyPort,
		StagingDir:         request.StagingDir,
		WorktreeDir:        request.WorktreeDir,
		PayloadJSON:        request.PayloadJSON,
		TaskJSON:           request.TaskJSON,
		Readonly:           request.Readonly,
		Interactive:        request.Interactive,
		InstructionsJSON:   request.InstructionsJSON,
		SecretNamespace:    request.SecretNamespace,
		TaskYAML:           request.TaskYAML,
		EnvironmentYAML:    request.EnvironmentYAML,
		Model:              request.Model,
		InvokedRole:        request.InvokedRole,
		InvokedName:        request.InvokedName,
		InvokedType:        request.InvokedType,
	}
}

func toDispatchHookFiles(files []orchestrator.HookFile) []HookFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]HookFile, len(files))
	for i, f := range files {
		out[i] = HookFile{Source: f.Source, TargetName: f.TargetName}
	}
	return out
}

func toCommandDefs(cmds map[string]orchestrator.CommandDef) map[string]CommandDef {
	if len(cmds) == 0 {
		return nil
	}
	out := make(map[string]CommandDef, len(cmds))
	for name, def := range cmds {
		out[name] = CommandDef{
			Name:               def.Name,
			Path:               def.Path,
			AllowedPatterns:    def.AllowedPatterns,
			DeniedPatterns:     def.DeniedPatterns,
			AllowedSubcommands: def.AllowedSubcommands,
			AllowStdin:         def.AllowStdin,
			Env:                def.Env,
		}
	}
	return out
}

func toBindMounts(bindings []orchestrator.BindMount) []BindMount {
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

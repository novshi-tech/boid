package orchestrator

import (
	"context"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

type dispatchBackend interface {
	Dispatch(ctx context.Context, plan *dispatcher.DispatchPlan) (string, error)
	WaitForJobCtx(ctx context.Context, jobID string) (dispatcher.JobCompletionResult, error)
}

// DispatchAdapter adapts a job dispatcher to hook/gate execution interfaces.
type DispatchAdapter struct {
	dispatcher dispatchBackend
	planner    *DispatchPlanner
}

func NewDispatchAdapter(dispatcher dispatchBackend, planner *DispatchPlanner) *DispatchAdapter {
	return &DispatchAdapter{dispatcher: dispatcher, planner: planner}
}

func (a *DispatchAdapter) ExecuteHook(ctx context.Context, event *HookFireEvent) (string, error) {
	request, err := a.planner.PlanHook(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, toDispatcherPlan(request))
}

func (a *DispatchAdapter) ExecuteGate(ctx context.Context, event *GateFireEvent) (string, error) {
	request, err := a.planner.PlanGate(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, toDispatcherPlan(request))
}

func (a *DispatchAdapter) WaitForJob(ctx context.Context, jobID string) (JobCompletion, error) {
	result, err := a.dispatcher.WaitForJobCtx(ctx, jobID)
	return JobCompletion{
		JobID:    jobID,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, err
}

type DBProjectCatalog struct {
	DB DBTX
}

func (c DBProjectCatalog) GetProject(id string) (*Project, error) {
	return GetProject(c.DB, id)
}

func (c DBProjectCatalog) ListProjects() ([]*Project, error) {
	return ListProjects(c.DB)
}

type DBTaskLookup struct {
	DB DBTX
}

func (l DBTaskLookup) GetTask(id string) (*Task, error) {
	return GetTask(l.DB, id)
}

func toDispatcherPlan(request *DispatchRequest) *dispatcher.DispatchPlan {
	if request == nil {
		return nil
	}
	return &dispatcher.DispatchPlan{
		TaskID:             request.TaskID,
		ProjectID:          request.ProjectID,
		HandlerID:          request.HandlerID,
		Role:               string(request.Role),
		ProjectDir:         request.ProjectDir,
		HomeDir:            request.HomeDir,
		HooksDir:           request.HooksDir,
		HookScript:         request.HookScript,
		BoidBinary:         request.BoidBinary,
		ServerSocket:       request.ServerSocket,
		Env:                request.Env,
		HostCommands:       toDispatcherCommands(request.HostCommands),
		AdditionalBindings: toDispatcherBindings(request.AdditionalBindings),
		WorkspaceDirs:      request.WorkspaceDirs,
		ProxyPort:          request.ProxyPort,
		StagingDir:         request.StagingDir,
		WorktreeDir:        request.WorktreeDir,
		PayloadJSON:        request.PayloadJSON,
		TaskJSON:           request.TaskJSON,
		Readonly:           request.Readonly,
	}
}

func toDispatcherCommands(cmds map[string]CommandDef) map[string]dispatcher.CommandDef {
	if len(cmds) == 0 {
		return nil
	}
	out := make(map[string]dispatcher.CommandDef, len(cmds))
	for name, def := range cmds {
		out[name] = dispatcher.CommandDef{
			Name:                def.Name,
			Path:                def.Path,
			AllowedPatterns:     def.AllowedPatterns,
			DeniedPatterns:      def.DeniedPatterns,
			AllowedSubcommands:  def.AllowedSubcommands,
			AllowStdin:          def.AllowStdin,
			Env:                 def.Env,
			ExtractSubcommandFn: def.ExtractSubcommandFn,
			RequireCwd:          def.RequireCwd,
			AllowedCwdPrefixes:  def.AllowedCwdPrefixes,
		}
	}
	return out
}

func toDispatcherBindings(bindings []BindMount) []dispatcher.BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]dispatcher.BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, dispatcher.BindMount{
			Source: binding.Source,
			Mode:   binding.Mode,
		})
	}
	return out
}

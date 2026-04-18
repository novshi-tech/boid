package server

import (
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func newCommandBroker(broker *sandbox.Broker) dispatcher.CommandBroker {
	if broker == nil {
		return nil
	}
	return &sandboxBrokerAdapter{broker: broker}
}

type sandboxBrokerAdapter struct {
	broker *sandbox.Broker
}

func (a *sandboxBrokerAdapter) RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx dispatcher.BrokerContext, resolve dispatcher.SecretResolver) string {
	tokenCtx := sandbox.TokenContext{
		JobID:             ctx.JobID,
		TaskID:            ctx.TaskID,
		ProjectID:         ctx.ProjectID,
		WorkspaceID:       ctx.WorkspaceID,
		AllowedProjectIDs: append([]string(nil), ctx.AllowedProjectIDs...),
		Role:              ctx.Role,
		ProjectDir:        ctx.ProjectDir,
		WorktreeDir:       ctx.WorktreeDir,
	}
	sandboxCommands := toSandboxCommandDefs(commands)
	if resolve != nil {
		return a.broker.RegisterWithSecrets(sandboxCommands, builtinPolicies, tokenCtx, func(key string) (string, error) {
			return resolve(key)
		})
	}
	return a.broker.Register(sandboxCommands, builtinPolicies, tokenCtx)
}

func (a *sandboxBrokerAdapter) UnregisterCommandToken(token string) {
	a.broker.Unregister(token)
}

func (a *sandboxBrokerAdapter) SocketPath() string {
	return a.broker.SocketPath
}

func toSandboxCommandDefs(commands map[string]orchestrator.CommandDef) map[string]sandbox.CommandDef {
	if len(commands) == 0 {
		return nil
	}
	out := make(map[string]sandbox.CommandDef, len(commands))
	for name, def := range commands {
		out[name] = sandbox.CommandDef{
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

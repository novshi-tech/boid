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

func (a *sandboxBrokerAdapter) RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, resolve dispatcher.SecretResolver) string {
	sandboxCommands := toSandboxCommandDefs(commands)
	if resolve != nil {
		return a.broker.RegisterWithSecrets(sandboxCommands, builtinPolicies, ctx, func(key string) (string, error) {
			return resolve(key)
		})
	}
	return a.broker.Register(sandboxCommands, builtinPolicies, ctx)
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

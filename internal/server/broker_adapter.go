package server

import (
	"github.com/novshi-tech/boid/internal/dispatcher"
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

func (a *sandboxBrokerAdapter) RegisterCommands(commands map[string]dispatcher.CommandDef, ctx dispatcher.BrokerContext, resolve dispatcher.SecretResolver) string {
	tokenCtx := sandbox.TokenContext{
		JobID:     ctx.JobID,
		TaskID:    ctx.TaskID,
		ProjectID: ctx.ProjectID,
		Role:      ctx.Role,
	}
	if resolve != nil {
		return a.broker.RegisterWithSecrets(commands, tokenCtx, func(key string) (string, error) {
			return resolve(key)
		})
	}
	return a.broker.Register(commands, tokenCtx)
}

func (a *sandboxBrokerAdapter) UnregisterCommandToken(token string) {
	a.broker.Unregister(token)
}

func (a *sandboxBrokerAdapter) SocketPath() string {
	return a.broker.SocketPath
}

package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// SecretResolver resolves a secret key into its plaintext value.
type SecretResolver func(key string) (string, error)

// CommandBroker is the dispatcher-owned behavior contract for host command brokering.
// The execution context is the canonical sandbox.TokenContext so adapters do not
// need to translate between dispatcher- and sandbox-side context shapes.
type CommandBroker interface {
	RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, resolve SecretResolver) string
	UnregisterCommandToken(token string)
	SocketPath() string
}

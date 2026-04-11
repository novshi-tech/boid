package dispatcher

import "github.com/novshi-tech/boid/internal/sandbox"

// SecretResolver resolves a secret key into its plaintext value.
type SecretResolver func(key string) (string, error)

// BrokerContext carries the dispatcher-side execution context associated with a broker token.
type BrokerContext struct {
	JobID             string
	TaskID            string
	ProjectID         string
	WorkspaceID       string
	AllowedProjectIDs []string
	Role              string
	ProjectDir        string
	WorktreeDir       string
}

// CommandBroker is the dispatcher-owned behavior contract for host command brokering.
type CommandBroker interface {
	RegisterCommands(commands map[string]CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx BrokerContext, resolve SecretResolver) string
	UnregisterCommandToken(token string)
	SocketPath() string
}

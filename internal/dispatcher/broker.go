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
	// RegisterCommands takes the short-name-keyed view of resolved host
	// commands (ResolveHostCommands' byName return value —
	// docs/plans/phase5-shim-and-task-context.md, "5a: shim
	// 固定ディレクトリ化" PR1), not the absolute-path-keyed view used for shim
	// bind-mount targets. The sandbox-side broker accepts the absolute path
	// too, as a compatibility fallback for the shim's current
	// ExecRequest.Command until 5a-2 switches it to send the short name.
	RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, resolve SecretResolver) string
	UnregisterCommandToken(token string)
	SocketPath() string
}

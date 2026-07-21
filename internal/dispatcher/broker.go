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
	// 固定ディレクトリ化" PR1). As of the 5a-3 cutover (PR3) this is the sole
	// broker key — the shim always sends the declared short name as
	// ExecRequest.Command (sandbox.CommandFromArgv0 — every shim's
	// bind-mount basename equals its declared name by construction under
	// sandboxShimBinDir), and the broker's pre-5a-3 absolute-path Path-scan
	// fallback was dropped in the same change.
	RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, resolve SecretResolver) string
	UnregisterCommandToken(token string)
	SocketPath() string
}

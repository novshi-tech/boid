package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// PoliciesToSandbox converts the orchestrator-owned neutral BuiltinPolicy
// representation into the sandbox-layer BuiltinPolicy the broker understands.
// dispatcher is the only layer allowed to bridge both sides.
func PoliciesToSandbox(in map[string]orchestrator.BuiltinPolicy) map[string]sandbox.BuiltinPolicy {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]sandbox.BuiltinPolicy, len(in))
	for name, p := range in {
		var ops map[string]struct{}
		if len(p.AllowedOps) > 0 {
			ops = make(map[string]struct{}, len(p.AllowedOps))
			for _, op := range p.AllowedOps {
				ops[op] = struct{}{}
			}
		}
		out[name] = sandbox.BuiltinPolicy{
			AllowedOps:      ops,
			AllowedCwdRoots: append([]string(nil), p.AllowedCwdRoots...),
		}
	}
	return out
}

package orchestrator

import "github.com/novshi-tech/boid/internal/sandbox"

// DefaultBuiltinPolicies creates per-command BuiltinPolicy values for the given role and
// command names. Call mergeBuiltinCommands before this to ensure "boid" is included.
func DefaultBuiltinPolicies(role Role, names []string) map[string]sandbox.BuiltinPolicy {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]sandbox.BuiltinPolicy, len(names))
	for _, name := range names {
		out[name] = policyFor(role, name)
	}
	return out
}

func policyFor(role Role, name string) sandbox.BuiltinPolicy {
	switch name {
	case "boid":
		return boidPolicy(role)
	case "git":
		return gitPolicy(role)
	default:
		return sandbox.BuiltinPolicy{}
	}
}

func boidPolicy(role Role) sandbox.BuiltinPolicy {
	switch role {
	case RoleHook:
		return sandbox.BuiltinPolicy{AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpJobDone): {},
			string(sandbox.BoidOpTaskGet): {},
		}}
	default: // RoleGate or empty → gate 相当
		return sandbox.BuiltinPolicy{AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpJobDone):    {},
			string(sandbox.BoidOpTaskCreate): {},
			string(sandbox.BoidOpTaskUpdate): {},
			string(sandbox.BoidOpTaskImport): {},
			string(sandbox.BoidOpTaskReopen): {},
		}}
	}
}

func gitPolicy(role Role) sandbox.BuiltinPolicy {
	switch role {
	case RoleHook:
		// hook からの broker 経由 git 操作 (fetch/push) は禁止。
		// agent はホスト側のリモートに直接アクセスすべきでない。
		return sandbox.BuiltinPolicy{}
	default: // RoleGate or empty → gate 相当
		return sandbox.BuiltinPolicy{AllowedOps: map[string]struct{}{
			string(sandbox.GitOpFetch): {},
			string(sandbox.GitOpPush):  {},
		}}
	}
}

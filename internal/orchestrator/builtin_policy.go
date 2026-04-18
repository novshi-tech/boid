package orchestrator

import "github.com/novshi-tech/boid/internal/sandbox"

// PolicyContext carries non-role data needed to compute role-derived policies.
// Currently only ProjectDir is used (gate boid policy lets gate jobs target
// the host project dir as cwd because gate sandboxes do not mount it under
// the entry root). Kept as a struct so additional context can be added without
// further signature churn.
type PolicyContext struct {
	ProjectDir string
}

// DefaultBuiltinPolicies creates per-command BuiltinPolicy values for the given role and
// command names. Call mergeBuiltinCommands before this to ensure "boid" is included.
func DefaultBuiltinPolicies(role Role, names []string, pctx PolicyContext) map[string]sandbox.BuiltinPolicy {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]sandbox.BuiltinPolicy, len(names))
	for _, name := range names {
		out[name] = policyFor(role, name, pctx)
	}
	return out
}

func policyFor(role Role, name string, pctx PolicyContext) sandbox.BuiltinPolicy {
	switch name {
	case "boid":
		return boidPolicy(role, pctx)
	case "git":
		return gitPolicy(role)
	default:
		return sandbox.BuiltinPolicy{}
	}
}

func boidPolicy(role Role, pctx PolicyContext) sandbox.BuiltinPolicy {
	switch role {
	case RoleHook:
		return sandbox.BuiltinPolicy{AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpJobDone): {},
			string(sandbox.BoidOpTaskGet): {},
		}}
	default: // RoleGate or empty → gate 相当
		// Gate sandbox does not mount ProjectDir under the entry root, so
		// the broker needs an explicit allow-list of cwd roots that the
		// gate script may target. /tmp is the gate $HOME (tmpfs), and the
		// host ProjectDir is read by the gate via host-side file access.
		cwds := []string{"/tmp"}
		if pctx.ProjectDir != "" {
			cwds = append(cwds, pctx.ProjectDir)
		}
		return sandbox.BuiltinPolicy{
			AllowedOps: map[string]struct{}{
				string(sandbox.BoidOpJobDone):    {},
				string(sandbox.BoidOpTaskCreate): {},
				string(sandbox.BoidOpTaskUpdate): {},
				string(sandbox.BoidOpTaskImport): {},
				string(sandbox.BoidOpTaskReopen): {},
			},
			AllowedCwdRoots: cwds,
		}
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

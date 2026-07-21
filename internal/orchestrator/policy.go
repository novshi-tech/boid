package orchestrator

import (
	"sort"
	"strings"
)

// PolicyContext carries non-role data needed to compute role-derived policies.
// ProjectDir lets boid policy accept the host project dir as cwd.
// HomeDir accepts the sandbox HOME, which is the default WorkDir for hook jobs.
type PolicyContext struct {
	ProjectDir string
	HomeDir    string
}

// BuiltinPolicy is the orchestrator-owned, sandbox-agnostic policy type.
// dispatcher is responsible for translating this into sandbox.BuiltinPolicy
// before it reaches the broker.
//
// AllowedOps is a sorted slice (rather than a set) so the value is trivially
// comparable and serialisable across the orchestrator/dispatcher boundary.
type BuiltinPolicy struct {
	AllowedOps      []string
	AllowedCwdRoots []string
}

// Allows reports whether op is in the allowed set.
func (p BuiltinPolicy) Allows(op string) bool {
	for _, a := range p.AllowedOps {
		if a == op {
			return true
		}
	}
	return false
}

// AllowsCwd reports whether cwd is within any of the policy's additional cwd roots.
func (p BuiltinPolicy) AllowsCwd(cwd string) bool {
	for _, root := range p.AllowedCwdRoots {
		if root == "" {
			continue
		}
		if cwd == root {
			return true
		}
		if strings.HasPrefix(cwd, root+"/") {
			return true
		}
	}
	return false
}

// DefaultBuiltinPolicies creates per-command BuiltinPolicy values for the
// given role and command names. "boid" is always available as a builtin;
// pass it explicitly via names.
func DefaultBuiltinPolicies(role Role, names []string, pctx PolicyContext) map[string]BuiltinPolicy {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]BuiltinPolicy, len(names))
	for _, name := range names {
		out[name] = policyFor(role, name, pctx)
	}
	return out
}

func policyFor(role Role, name string, pctx PolicyContext) BuiltinPolicy {
	switch name {
	case "boid":
		return boidPolicy(role, pctx)
	case "fetch":
		return fetchPolicy(role, pctx)
	default:
		return BuiltinPolicy{}
	}
}

func boidPolicy(_ Role, pctx PolicyContext) BuiltinPolicy {
	cwds := []string{"/tmp"}
	if pctx.ProjectDir != "" {
		cwds = append(cwds, pctx.ProjectDir)
	}
	if pctx.HomeDir != "" {
		cwds = append(cwds, pctx.HomeDir)
	}
	return BuiltinPolicy{
		AllowedOps: sortedOps(
			OpBoidJobDone,
			OpBoidJobList,
			OpBoidJobShow,
			OpBoidJobLog,
			OpBoidActionSend,
			OpBoidAgentStop,
			OpBoidTaskCreate,
			OpBoidTaskGet,
			OpBoidTaskUpdate,
			OpBoidTaskImport,
			OpBoidTaskReopen,
			OpBoidTaskList,
			OpBoidTaskNotify,
			OpBoidTaskAnswer,
			OpBoidTaskAsk,
			OpBoidTaskDelete,
			OpBoidTaskCurrent,
			OpBoidTaskInstructions,
			OpBoidTaskEnv,
			OpBoidTaskPayload,
			OpBoidTaskAttachmentsList,
			OpBoidTaskAttachmentsGet,
			OpBoidTaskUpdatePayloadPatch,
		),
		AllowedCwdRoots: cwds,
	}
}

// fetchPolicy returns the policy for the fetch builtin (HTTP GET only).
// No cwd restriction is needed since fetch does not perform local filesystem
// operations; it is broker-mediated and the SSRF guard lives in the handler.
func fetchPolicy(_ Role, _ PolicyContext) BuiltinPolicy {
	return BuiltinPolicy{
		AllowedOps: sortedOps(OpFetchGet),
	}
}

func sortedOps(ops ...string) []string {
	out := append([]string(nil), ops...)
	sort.Strings(out)
	return out
}

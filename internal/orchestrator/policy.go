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
// given role and command names. "boid" and "git" are always available as
// builtins; pass them explicitly via names.
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
	case "git":
		return gitPolicy(role, pctx)
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
			OpBoidTaskDelete,
		),
		AllowedCwdRoots: cwds,
	}
}

func gitPolicy(_ Role, pctx PolicyContext) BuiltinPolicy {
	// AllowedCwdRoots は /tmp と自タスクの ProjectDir のみを許可する。
	// WorktreeRoot 配下は validateGitBuiltinCwd が独立して許可するため不要。
	// HomeDir は意図的に除外: HomeDir 配下には同一 workspace の peer project が
	// 存在し得る。HomeDir を追加すると peer project 配下の cwd が通り、
	// git plumbing 経由で peer project に書き込めてしまう (GitHub Issue 相当)。
	cwds := []string{"/tmp"}
	if pctx.ProjectDir != "" {
		cwds = append(cwds, pctx.ProjectDir)
	}
	return BuiltinPolicy{
		AllowedOps:      sortedOps(OpGitFetch, OpGitPush, OpGitPushDelete),
		AllowedCwdRoots: cwds,
	}
}

func sortedOps(ops ...string) []string {
	out := append([]string(nil), ops...)
	sort.Strings(out)
	return out
}

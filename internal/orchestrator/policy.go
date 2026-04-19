package orchestrator

import (
	"sort"
	"strings"
)

// PolicyContext carries non-role data needed to compute role-derived policies.
// ProjectDir lets gate boid policy accept the host project dir as cwd (gate
// sandboxes do not mount it under the entry root). HomeDir accepts the
// sandbox HOME, which is the default WorkDir for gate jobs (their
// Visibility.ProjectDir is empty, so resolveWorkDir falls back to HOME).
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

func boidPolicy(role Role, pctx PolicyContext) BuiltinPolicy {
	switch role {
	case RoleHook:
		return BuiltinPolicy{AllowedOps: sortedOps(OpBoidJobDone, OpBoidTaskGet)}
	default: // RoleGate or empty → gate 相当
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
				OpBoidTaskCreate,
				OpBoidTaskUpdate,
				OpBoidTaskImport,
				OpBoidTaskReopen,
			),
			AllowedCwdRoots: cwds,
		}
	}
}

func gitPolicy(role Role, pctx PolicyContext) BuiltinPolicy {
	switch role {
	case RoleHook:
		// hook からの broker 経由 git 操作 (fetch/push) は禁止。
		return BuiltinPolicy{}
	default:
		// gate sandbox は worktree を mount しないので、sandbox 内の cwd は
		// 必ずホスト側 worktree root と別名前空間になる。broker は
		// binding.WorktreeRoot で git を実行するため cwd 検証は冗長だが、
		// 他の builtin と同じポリシー構造を保つために AllowedCwdRoots を
		// HomeDir/ProjectDir/tmp で埋める (boid policy と同じ扱い)。
		cwds := []string{"/tmp"}
		if pctx.ProjectDir != "" {
			cwds = append(cwds, pctx.ProjectDir)
		}
		if pctx.HomeDir != "" {
			cwds = append(cwds, pctx.HomeDir)
		}
		return BuiltinPolicy{
			AllowedOps:      sortedOps(OpGitFetch, OpGitPush),
			AllowedCwdRoots: cwds,
		}
	}
}

func sortedOps(ops ...string) []string {
	out := append([]string(nil), ops...)
	sort.Strings(out)
	return out
}

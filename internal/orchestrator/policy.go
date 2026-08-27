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

// boidPolicy grants every hook/exec job the "boid" builtin's general op set.
//
// docs/plans/signal-ingest-detailed-design.md §3.2 (PR-3): OpBoidSignalList /
// OpBoidSignalAck are added below (the judgment-side scan/ack surface — any
// job may read its own workspace's signal inbox and ack a Signal once it has
// written a judgment for it). OpBoidSignalIngest / OpBoidSignalCursorGet are
// DELIBERATELY NOT added here — the design doc is explicit that granting
// those two is PR-5's job (a connector-scoped, reduced policy handed only to
// derived-trigger exec jobs), not this general policy. Do not "complete the
// set" by adding them here; see sandbox.BoidOpSignalIngest's own doc comment
// for the same note from the protocol side.
func boidPolicy(_ Role, pctx PolicyContext) BuiltinPolicy {
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
			OpBoidProjectBehaviors,
			OpBoidProjectList,
			OpBoidCardGet,
			OpBoidCardList,
			OpBoidTaskIdentityLink,
			OpBoidTaskIdentityUnlink,
			OpBoidTaskIdentityResolve,
			OpBoidTaskResolveOrCapture,
			OpBoidActionList,
			OpBoidSignalList,
			OpBoidSignalAck,
		),
		AllowedCwdRoots: boidCwdRoots(pctx),
	}
}

// boidCwdRoots is the "boid" builtin's AllowedCwdRoots — shared by
// boidPolicy and connectorPolicy (both grant the SAME cwd surface; only the
// op set differs between the general and connector-scoped policies).
func boidCwdRoots(pctx PolicyContext) []string {
	cwds := []string{"/tmp"}
	if pctx.ProjectDir != "" {
		cwds = append(cwds, pctx.ProjectDir)
	}
	if pctx.HomeDir != "" {
		cwds = append(cwds, pctx.HomeDir)
	}
	return cwds
}

// fetchPolicy returns the policy for the fetch builtin (HTTP GET only).
// No cwd restriction is needed since fetch does not perform local filesystem
// operations; it is broker-mediated and the SSRF guard lives in the handler.
func fetchPolicy(_ Role, _ PolicyContext) BuiltinPolicy {
	return BuiltinPolicy{
		AllowedOps: sortedOps(OpFetchGet),
	}
}

// ConnectorBuiltinPolicies returns the reduced, connector-scoped builtin
// policy map a signal-derived trigger's exec job gets INSTEAD OF
// DefaultBuiltinPolicies(RoleHook, []string{"boid","fetch"}, pctx)
// (docs/plans/signal-ingest-detailed-design.md §5.2, Q27):
//
//   - "boid" -> connectorPolicy(pctx): ONLY OpBoidSignalIngest /
//     OpBoidSignalCursorGet / OpBoidJobDone are allowed. Nothing else from
//     the general boidPolicy set (task_create, card/action reads, ...) is
//     reachable — a connector job cannot touch boid state beyond its own
//     source's ingest/cursor. OpBoidJobDone is included alongside those two
//     (rather than excluded like every other general op) because it is not
//     a "touch boid state" capability the §5.2 reduction is meant to deny —
//     handleBoidBuiltin (internal/sandbox/broker.go) restricts it to the
//     caller's OWN job id (entry.Context.JobID) regardless of policy, so
//     granting it cannot reach another job/project. Omitting it was a real
//     bug (found during shadow-a's 2026-08-27 2-hour observation): every
//     connector job's in-container runner posts `boid job done` to report
//     its own completion (internal/sandbox/runner/runner.go's postJobDone,
//     the same mechanism every other non-foreground job relies on) — with
//     job_done missing from AllowedOps, the broker rejected that call, and
//     the daemon's "runtime exited without boid job done" fallback then
//     forced every connector run's real (successful) exit_code=0 into a
//     recorded exit_code=1/JobStatusFailed, which trigger_loop.go's
//     trackFailStreak counted as a real failure on every single run.
//   - NO "fetch" entry at all (§5.2: "fetch builtin も渡さない —
//     connector の外部到達は gateway で足りる"): a connector job's only
//     network path is the API gateway (already reached via
//     $BOID_API_BASE/$BOID_SIGNAL_SERVICE), not the general-purpose fetch
//     builtin every other hook/exec job gets.
//
// Called only by dispatcher.BuildSessionJobSpec when
// SessionJobInput.ConnectorPolicy is true — every other job keeps calling
// DefaultBuiltinPolicies unchanged.
func ConnectorBuiltinPolicies(pctx PolicyContext) map[string]BuiltinPolicy {
	return map[string]BuiltinPolicy{"boid": connectorPolicy(pctx)}
}

// connectorPolicy is ConnectorBuiltinPolicies' "boid" entry — see that
// function's doc comment for the full rationale.
func connectorPolicy(pctx PolicyContext) BuiltinPolicy {
	return BuiltinPolicy{
		AllowedOps:      sortedOps(OpBoidSignalIngest, OpBoidSignalCursorGet, OpBoidJobDone),
		AllowedCwdRoots: boidCwdRoots(pctx),
	}
}

func sortedOps(ops ...string) []string {
	out := append([]string(nil), ops...)
	sort.Strings(out)
	return out
}

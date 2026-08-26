package orchestrator

import (
	"slices"
	"testing"
)

func TestDefaultBuiltinPolicies_HookBoidOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_HookBoidOps(t *testing.T) {
	boidP := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})["boid"]
	wantOps := []string{
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
	}
	if !opsEqual(boidP.AllowedOps, wantOps) {
		t.Errorf("hook×boid AllowedOps = %v, want %v", boidP.AllowedOps, wantOps)
	}
}

// TestDefaultBuiltinPolicies_HookBoidExcludesConnectorOnlySignalOps pins
// docs/plans/signal-ingest-detailed-design.md §3.2's explicit exception to
// the "new op → add it to boidPolicy" default: OpBoidSignalIngest /
// OpBoidSignalCursorGet are declared (protocol.go / policy_ops.go) but must
// NOT be reachable through the general boidPolicy in PR-3 — granting them is
// PR-5's job (a connector-scoped, reduced policy). Without this test, a
// future edit that "completes the set" by adding these two to boidPolicy's
// AllowedOps would silently regress that boundary.
func TestDefaultBuiltinPolicies_HookBoidExcludesConnectorOnlySignalOps(t *testing.T) {
	boidP := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})["boid"]
	if boidP.Allows(OpBoidSignalIngest) {
		t.Error("hook×boid must NOT allow signal_ingest (PR-5's connector-scoped policy grants it, not the general policy)")
	}
	if boidP.Allows(OpBoidSignalCursorGet) {
		t.Error("hook×boid must NOT allow signal_cursor_get (PR-5's connector-scoped policy grants it, not the general policy)")
	}
	if !boidP.Allows(OpBoidSignalList) {
		t.Error("hook×boid should allow signal_list")
	}
	if !boidP.Allows(OpBoidSignalAck) {
		t.Error("hook×boid should allow signal_ack")
	}
}

// hook×boid policy は cwd に /tmp, ProjectDir, HomeDir を許可する。
func TestDefaultBuiltinPolicies_HookBoidCwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	boidPolicy := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, pctx)["boid"]
	for _, cwd := range []string{"/tmp", "/tmp/sub", "/work/project", "/work/project/sub", "/home/user", "/home/user/.boid/output"} {
		if !boidPolicy.AllowsCwd(cwd) {
			t.Errorf("hook×boid should allow cwd %q, AllowedCwdRoots=%v", cwd, boidPolicy.AllowedCwdRoots)
		}
	}
	if boidPolicy.AllowsCwd("/etc") {
		t.Errorf("hook×boid should reject cwd /etc")
	}
}

func TestDefaultBuiltinPolicies_FetchPolicy(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"fetch"}, PolicyContext{})
	p, ok := policies["fetch"]
	if !ok {
		t.Fatal("missing fetch policy")
	}
	if !p.Allows(OpFetchGet) {
		t.Errorf("fetch policy should allow op %q", OpFetchGet)
	}
}

// hook×fetch policy is identical regardless of role (no special gate overrides).
func TestDefaultBuiltinPolicies_FetchRoleInvariant(t *testing.T) {
	pHook := DefaultBuiltinPolicies(RoleHook, []string{"fetch"}, PolicyContext{})["fetch"]
	pEmpty := DefaultBuiltinPolicies("", []string{"fetch"}, PolicyContext{})["fetch"]
	if !opsEqual(pHook.AllowedOps, pEmpty.AllowedOps) {
		t.Errorf("fetch policy should be role-invariant; hook=%v empty=%v", pHook.AllowedOps, pEmpty.AllowedOps)
	}
}

// --- ConnectorBuiltinPolicies (docs/plans/signal-ingest-detailed-design.md
// §5.2, PR-5, Q27) ---

func TestConnectorBuiltinPolicies_OnlySignalIngestAndCursorGet(t *testing.T) {
	policies := ConnectorBuiltinPolicies(PolicyContext{})
	boidP, ok := policies["boid"]
	if !ok {
		t.Fatal("missing boid policy")
	}
	wantOps := []string{OpBoidSignalIngest, OpBoidSignalCursorGet}
	if !opsEqual(boidP.AllowedOps, wantOps) {
		t.Errorf("connector boid AllowedOps = %v, want %v", boidP.AllowedOps, wantOps)
	}
}

// TestConnectorBuiltinPolicies_ExcludesGeneralBoidOps pins that NONE of the
// general boidPolicy ops (task_create, signal_list, card_get, ...) leak
// into the connector-scoped policy — a connector job cannot reach boid
// state beyond its own source's ingest/cursor.
func TestConnectorBuiltinPolicies_ExcludesGeneralBoidOps(t *testing.T) {
	boidP := ConnectorBuiltinPolicies(PolicyContext{})["boid"]
	forbidden := []string{
		OpBoidTaskCreate, OpBoidTaskGet, OpBoidTaskUpdate, OpBoidCardGet, OpBoidCardList,
		OpBoidSignalList, OpBoidSignalAck, OpBoidActionSend, OpBoidActionList,
		OpBoidProjectList, OpBoidProjectBehaviors,
	}
	for _, op := range forbidden {
		if boidP.Allows(op) {
			t.Errorf("connector policy must NOT allow %q (general boidPolicy op leaking into the reduced connector policy)", op)
		}
	}
}

// TestConnectorBuiltinPolicies_NoFetchEntry pins §5.2's "fetch builtin も
// 渡さない" — unlike DefaultBuiltinPolicies(RoleHook, []string{"boid",
// "fetch"}, ...), ConnectorBuiltinPolicies never returns a "fetch" key at
// all (not even an empty one) for BuildSessionJobSpec to select.
func TestConnectorBuiltinPolicies_NoFetchEntry(t *testing.T) {
	policies := ConnectorBuiltinPolicies(PolicyContext{})
	if _, ok := policies["fetch"]; ok {
		t.Error(`ConnectorBuiltinPolicies must not return a "fetch" entry (§5.2: connector の外部到達は gateway で足りる)`)
	}
	if len(policies) != 1 {
		t.Errorf("ConnectorBuiltinPolicies returned %d entries, want exactly 1 (\"boid\")", len(policies))
	}
}

func TestConnectorBuiltinPolicies_CwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	boidP := ConnectorBuiltinPolicies(pctx)["boid"]
	for _, cwd := range []string{"/tmp", "/work/project", "/home/user"} {
		if !boidP.AllowsCwd(cwd) {
			t.Errorf("connector policy should allow cwd %q, AllowedCwdRoots=%v", cwd, boidP.AllowedCwdRoots)
		}
	}
	if boidP.AllowsCwd("/etc") {
		t.Error("connector policy should reject cwd /etc")
	}
}

func opsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, op := range a {
		if !slices.Contains(b, op) {
			return false
		}
	}
	return true
}

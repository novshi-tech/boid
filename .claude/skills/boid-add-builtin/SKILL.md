---
name: boid-add-builtin
description: >
  A checklist for adding a new builtin command (e.g. oci, net) to the boid orchestrator.
  Guides the end-to-end workflow: implementing the builtin command, registering it in the
  policy table, adding broker dispatch, and writing tests.
  Use when a team member wants to add a new builtin command to the boid orchestrator.
---

# boid builtin command — Adding a New Command or Op

For a quick look at code details and file paths, see [references/key-files.md](references/key-files.md).

`boid` and `fetch` are always available without any declaration in project.yaml / kit.yaml.
New builtins follow the same convention — they are always injected by the planner
(the `builtin_commands` config key has been removed).

## Which path applies to you?

There are two different things people mean by "add a builtin", and they need
different checklists. **Check which one you actually need before following
either walkthrough below** — this distinction was missing from this skill
until 2026-08-26 (docs/plans/signal-ingest-detailed-design.md PR-3), and
following the wrong one wastes real time (or worse, ships an op with no
escape-manifest test coverage, silently weakening the security gate).

1. **Adding a new OP to the already-registered `boid` builtin** — e.g.
   `card_get`/`card_list`, `action_list`, `task_identity_link`, and most
   recently `signal_list`/`signal_ack`/`signal_ingest`/`signal_cursor_get`.
   **This is the common case** — almost every real PR in this repo's history
   that touches this area is this path, not the one below. See "Adding a new
   OP to the existing `boid` builtin" immediately below.
2. **Adding an entirely new top-level builtin command** (a new name alongside
   `boid`/`fetch`, e.g. a hypothetical `oci` or `net`) — rare; no builtin has
   actually been added this way since `fetch`. See "Steps 1–7" further down.

---

## Adding a new OP to the existing `boid` builtin (the common case)

Follow these steps in order. Each names the exact file and what changes; see
[references/key-files.md](references/key-files.md)'s first table for the
full file-by-file rationale.

1. **`internal/sandbox/protocol.go`** — add a `BoidOp` constant (e.g.
   `BoidOpFooBar BoidOp = "foo_bar"`) and any new `BoidRequest` fields the op
   needs. Write a doc comment explaining what the op does and how it's scoped
   — this codebase's convention is that every op constant carries its own
   design rationale inline (read a few existing ones for the tone).
2. **`internal/orchestrator/policy_ops.go`** — add the MIRROR string constant
   (`OpBoidFooBar = "foo_bar"`), byte-identical to step 1's value.
   `orchestrator` cannot import `sandbox` (layer direction), so this string
   literal has to be kept in lock-step by hand — that's exactly what step 7
   below checks.
3. **`internal/orchestrator/policy.go`** — decide whether `boidPolicy()`
   should grant this op to every hook/exec job (the default — add
   `OpBoidFooBar` to its `sortedOps(...)` call) or whether it's meant to be
   restricted to a narrower policy that doesn't exist yet (rare — see
   `signal_ingest`/`signal_cursor_get`'s doc comment there for a real example
   of "declared but deliberately not granted here"). Don't reflexively
   "complete the set" without checking which one applies.
4. **`internal/sandbox/boid_shim.go`** — add a `case` in `parseBoidRequest`'s
   command/subcommand switch and a `parseBoidFooBar(args []string)
   (*BoidRequest, error)` function that turns `boid foo bar [flags]` into a
   `*BoidRequest{Op: BoidOpFooBar, ...}`. This is the CLI surface a sandboxed
   agent actually types; skipping it leaves the op reachable only by a
   hand-crafted request, which no real caller sends.
5. **`internal/sandbox/broker.go`** — add a `case BoidOpFooBar:` in
   `handleBoidBuiltin`'s `switch boidReq.Op`. Validate required fields and,
   if the op needs workspace/project scoping, do it HERE and make it
   broker-authoritative — inject the value from `entry.Context`, never trust
   whatever the request already carries (this exact class of bug — scoping
   left to the executor alone, leaving a flag like `--workspace-id`
   unchecked — has been caught by review multiple times; grep this file for
   "codex/Opus レビュー" for the incident write-ups).
6. **`internal/server/boid_executor.go`** — add a `case sandbox.BoidOpFooBar:`
   in `boidBuiltinExecutor.ExecuteBoidBuiltin`'s `switch req.Op` with the
   actual business logic. **This is NOT the same file as
   `internal/sandbox/boid_executor.go`** — that one is a 13-line interface
   declaration (`BoidExecutor`) with nothing to add to. If the op needs a new
   backing store/service dependency that no existing constructor parameter
   provides, follow the `resolveOrCaptureService`/`actionListService`
   "runtime interface-check against the existing `workflow` parameter"
   pattern already used in this file, rather than adding a new required
   constructor parameter (which breaks every existing call site, including
   ones outside your PR's scope).
7. **Tests — three separate registries, not just op-specific test files:**
   - `internal/orchestrator/policy_test.go`'s `TestDefaultBuiltinPolicies_HookBoidOps`
     — add the new `OpBoidFooBar` to `wantOps`. This is a plain
     `len(a) != len(b)` comparison (`opsEqual`), so a forgotten entry fails
     immediately as a length mismatch, not a silent pass.
   - `internal/dispatcher/policy_translate_test.go`'s `TestOpConstantsMirror`
     — add the `{orchestrator.OpBoidFooBar, string(sandbox.BoidOpFooBar)}`
     pair, closing the loop on step 2's mirror.
   - `internal/sandbox/broker_op_escape_test.go`'s `opEscapeCoverage` map —
     add an entry keyed by the Go constant name (`"BoidOpFooBar"`) naming a
     real test function that drives the op through the broker's policy gate
     (`assertBoidOpRejectedByPolicy` is the standard one-liner for this — see
     any existing `TestBroker_Boid*_PolicyReject` for the pattern) or an
     `exempt` reason. **This is an AST-driven hard fail**: it parses every
     `BoidOp` constant straight out of `protocol.go`, so forgetting this step
     fails the build with the exact missing op named — there is no way to
     silently skip it, but there IS a way to not know it exists if you only
     read the older parts of this skill, which is why this step used to be
     missing from here entirely until 2026-08-26.
   - Plus the usual op-specific tests: shim parsing (`boid_shim_*_test.go`),
     broker scoping (`broker_*_test.go`), and executor logic
     (`internal/server/boid_executor_*_test.go`).

---

## Adding an entirely new top-level builtin command (rare)

Follow these **7 steps** in order when adding a brand-new builtin name (not a
new op on `boid`). Each step lists the relevant existing implementation to
reference.

> **Historical note**: `git` used to be a builtin as well. The git gateway cutover (2026-07)
> retired the sandbox-side `git` builtin — `git` is now a credential-less binary running inside
> the sandbox, and all fetch/push traffic goes through the daemon-hosted git gateway (auth-injecting
> reverse proxy). Removing the `git` builtin is why the examples below reference `boid` and `fetch`
> as the current-generation reference implementations.

---

## Step 1 — Add the protocol

Add the following to `internal/sandbox/protocol.go`:

```go
// New Op type and constants
type OciOp string

const (
    OciOpRun OciOp = "run"
)

// Add a new field to ExecRequest
type ExecRequest struct {
    // ... existing fields ...
    Oci *OciRequest `json:"oci,omitempty"`
}

// Define the request type
type OciRequest struct {
    Op    OciOp  `json:"op"`
    Image string `json:"image,omitempty"`
    // ...
}
```

Reference: the `BoidOp` and `GitOp` definition patterns.

---

## Step 2 — Implement the handler

Create `internal/sandbox/oci_builtin.go` and follow these constraints:

```go
func handleOciBuiltinRequest(req *ExecRequest, entry *tokenEntry) *ExecResponse {
    // Always check policy first (skipping this disables op restrictions)
    if !entry.hasBuiltinPolicy("oci") {
        return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: oci"}
    }
    // Validate cwd (see validateBoidBuiltinCwd in broker.go for reference)
    if err := validateOciBuiltinCwd(req.Cwd, entry); err != nil {
        return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
    }
    // Check op restriction (do NOT branch on role here — keep it in the policy table)
    if !entry.allowsBuiltinOp("oci", string(req.Oci.Op)) {
        return &ExecResponse{
            ExitCode: 1,
            Stderr:   fmt.Sprintf("oci op %q not allowed for role %s", req.Oci.Op, entry.Context.Role),
        }
    }
    // Observability: log role at execution time (read-only; never use it for authorization)
    slog.Info("oci builtin run requested", "role", entry.Context.Role)
    // ... main logic ...
}
```

**Prohibited**: branching directly on `entry.Context.Role == "hook"` or any other role string.
Role-based authorization is fully handled by the planner's `DefaultBuiltinPolicies`; the broker
only consults that result.

Reference: `handleBoidBuiltin` in `internal/sandbox/broker.go` (and `handleFetchBuiltin` in `internal/sandbox/fetch_builtin.go` for a smaller, single-op builtin).

---

## Step 3 — Define the policy (critical)

Edit `internal/orchestrator/policy.go`:

```go
func policyFor(role Role, name string, pctx PolicyContext) BuiltinPolicy {
    switch name {
    case "boid":
        return boidPolicy(role, pctx)
    case "fetch":
        return fetchPolicy(role, pctx)
    case "oci":
        return ociPolicy(role, pctx) // add this
    default:
        return BuiltinPolicy{}
    }
}

func ociPolicy(_ Role, pctx PolicyContext) BuiltinPolicy {
    // All roles share the same policy (no role branching).
    // If role-specific restrictions become necessary in the future,
    // replace `_ Role` with `role Role` and add a switch here.
    cwds := []string{"/tmp"}
    if pctx.ProjectDir != "" {
        cwds = append(cwds, pctx.ProjectDir)
    }
    if pctx.HomeDir != "" {
        cwds = append(cwds, pctx.HomeDir)
    }
    return BuiltinPolicy{
        AllowedOps:      sortedOps(string(OciOpRun)),
        AllowedCwdRoots: cwds,
    }
}
```

**Required**:
- Write a **rationale comment** explaining why ops are allowed (and under what constraints)
- Default: all roles share the same policy (`_ Role`). Only add a role `switch` when role-specific
  restrictions are genuinely needed — current `boid` and `fetch` builtins are role-agnostic as a reference
- Note any security concerns or related issues alongside the policy

---

## Step 4 — Wire into the builtin lists

The builtin name list is injected in **two** places; add the new name to both. Both pass
`RoleHook` — there is no separate "gate" role (that mechanism was removed).

1. `PlanHook` in `internal/orchestrator/planner.go` (the task-hook dispatch path):

```go
BuiltinPolicies: DefaultBuiltinPolicies(
    RoleHook,
    []string{"boid", "fetch", "oci"},
    PolicyContext{ProjectDir: proj.WorkDir, HomeDir: sandboxHomeDir()},
),
```

2. `BuildSessionJobSpec` in `internal/dispatcher/session_job.go` (the session / `boid exec`
   path — `BuildExecJobSpec` reuses it):

```go
builtinPolicies := orchestrator.DefaultBuiltinPolicies(
    orchestrator.RoleHook,
    []string{"boid", "fetch"},
    orchestrator.PolicyContext{ProjectDir: input.ProjectWorkDir},
)
```

Then add the new builtin name to `validateBuiltinHostConflict` in `internal/orchestrator/spec_loader.go` to prevent re-declaring it via `host_commands`.

---

## Step 5 — Add the broker dispatch branch

Add a branch inside `Handle()` in `internal/sandbox/broker.go`:

```go
func (b *Broker) Handle(req *ExecRequest) *ExecResponse {
    // ...
    if req.Command == "oci" {
        if entry.hasBuiltinPolicy("oci") {
            return handleOciBuiltinRequest(req, entry)
        }
        if def, ok := entry.Commands["oci"]; ok {
            return b.execCommand(req, def, entry)
        }
        return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: oci"}
    }

    // ...
}
```

**If a binding capture is needed** (add at the top of `Register()`):

```go
if entry.hasBuiltinPolicy("oci") {
    var err error
    entry.Oci, err = captureOciBinding(ctx.ProjectDir)
    logOciBindingSnapshot(ctx, entry.Oci, err)
}
```

Add a capture only when the builtin references external URLs or resources that must be protected
from tampering — the pattern is a trusted snapshot taken at token-registration time so an agent
cannot tamper with the value later. (The retired `git` builtin's `captureGitBinding` was the
canonical example of this pattern before the git gateway cutover replaced it with a URL rewrite at
the gateway layer.)

---

## Step 6 — Tests

### 6a. Add matrix tests to `policy_test.go`

```go
// hook×oci must allow the run op.
func TestDefaultBuiltinPolicies_HookOciHasRun(t *testing.T) {
    policies := DefaultBuiltinPolicies(RoleHook, []string{"oci"}, PolicyContext{})
    if !policies["oci"].Allows(string(OciOpRun)) {
        t.Errorf("hook×oci should allow run, got %v", policies["oci"].AllowedOps)
    }
}

// oci policy is identical regardless of role (no role branching). Mirrors the
// existing TestDefaultBuiltinPolicies_FetchRoleInvariant convention: the empty
// test-only role must resolve to the same ops as RoleHook.
func TestDefaultBuiltinPolicies_OciRoleInvariant(t *testing.T) {
    pHook := DefaultBuiltinPolicies(RoleHook, []string{"oci"}, PolicyContext{})["oci"]
    pEmpty := DefaultBuiltinPolicies("", []string{"oci"}, PolicyContext{})["oci"]
    if !opsEqual(pHook.AllowedOps, pEmpty.AllowedOps) {
        t.Errorf("oci policy should be role-invariant; hook=%v empty=%v", pHook.AllowedOps, pEmpty.AllowedOps)
    }
}
```

### 6b. Create `oci_builtin_test.go`

Minimum scenarios to cover:

- A forbidden op is rejected (empty policy or policy that does not include the op)
- An allowed op passes through
- Missing or out-of-range cwd returns an error

Reference: test helpers in `internal/sandbox/broker_test.go` and `internal/sandbox/fetch_builtin_test.go` for token registration, policy plumbing, and cwd validation patterns.

---

## Step 7 — Security checklist

Check these items when the new builtin involves external communication or host resource access:

- [ ] **Least privilege**: does hook receive more permissions than necessary? Hook should be read-only / notify-only by default
- [ ] **Workspace isolation**: does the implementation respect existing workspace isolation (e.g. `entry.Context.AllowedProjectIDs`)?
- [ ] **Secret leakage**: do error messages or slog calls expose secrets or credentials?
- [ ] **Trusted snapshot**: can an agent tamper with external URLs or resource references? Capture a snapshot at token registration time (as the retired `captureGitBinding` did) or move the enforcement to a proxy layer (as the git gateway does).
- [ ] **Minimal host access**: are host resources (files, network, processes) used to the minimum required?
- [ ] **Role-invariant test**: is there a `RoleInvariant` equivalent test confirming the empty-role policy matches the hook-role policy? (see `TestDefaultBuiltinPolicies_FetchRoleInvariant`)

---

## Design principles

| Principle | Rationale |
|-----------|-----------|
| broker has no knowledge of role | Role decisions are centralized in `DefaultBuiltinPolicies`; broker consults only the policy table |
| policy is stamped at registration | Eliminates role evaluation at dispatch time; keeps broker simple |
| empty role == hook role | Policies are role-invariant, so the empty test-only role resolves to the same policy as `RoleHook`. There is no separate "gate" role — that mechanism was removed |
| no role branching by default | Current `boid` and `fetch` builtins are role-agnostic. Add a role `switch` only when a new builtin genuinely needs role-specific restrictions |

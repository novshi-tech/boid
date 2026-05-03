---
name: boid-add-builtin
description: >
  A checklist for adding a new builtin command (e.g. oci, net) to the boid orchestrator.
  Guides the end-to-end workflow: implementing the builtin command, registering it in the
  policy table, adding broker dispatch, and writing tests.
  Use when a team member wants to add a new builtin command to the boid orchestrator.
---

# boid builtin command — Adding a New Command

Follow these **7 steps** in order when adding a new builtin.
Each step lists the relevant existing implementation to reference.

For a quick look at code details and file paths, see [references/key-files.md](references/key-files.md).

`git` and `boid` are always available without any declaration in project.yaml / kit.yaml.
New builtins follow the same convention — they are always injected by the planner
(the `builtin_commands` config key has been removed).

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
    // Validate cwd (see validateGitBuiltinCwd in git_builtin.go for reference)
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

Reference: `handleGitBuiltinRequest` in `internal/sandbox/git_builtin.go`.

---

## Step 3 — Define the policy (critical)

Edit `internal/orchestrator/builtin_policy.go`:

```go
func policyFor(role Role, name string) sandbox.BuiltinPolicy {
    switch name {
    case "boid":
        return boidPolicy(role)
    case "git":
        return gitPolicy(role)
    case "oci":
        return ociPolicy(role) // add this
    default:
        return sandbox.BuiltinPolicy{}
    }
}

func ociPolicy(role Role) sandbox.BuiltinPolicy {
    switch role {
    case RoleHook:
        // Container execution from hook is forbidden.
        // Prevents the agent from directly manipulating host-side resources.
        // Related: same design principle as the git builtin hook restriction.
        return sandbox.BuiltinPolicy{}
    default: // RoleGate or empty → treat as gate
        // Gate needs container execution for validation/build purposes, so run is allowed.
        // default (empty role) mirrors gate policy by convention (test compatibility).
        return sandbox.BuiltinPolicy{AllowedOps: map[string]struct{}{
            string(sandbox.OciOpRun): {},
        }}
    }
}
```

**Required**:
- Write a **rationale comment** in each case (why the op is allowed or forbidden)
- The `default` case (empty role) must use the **same policy as gate** (test-compatibility convention)
- Note any security concerns or related issues alongside the policy

---

## Step 4 — Wire into the planner

Add the new builtin to the builtin lists in `PlanHook` / `PlanGate` (`internal/orchestrator/planner.go`):

```go
// PlanHook
BuiltinPolicies: DefaultBuiltinPolicies(RoleHook,
    []string{"boid", "git", "oci"},
    PolicyContext{ProjectDir: proj.WorkDir},
),
// PlanGate
BuiltinPolicies: DefaultBuiltinPolicies(RoleGate,
    []string{"boid", "git", "oci"},
    PolicyContext{ProjectDir: proj.WorkDir},
),
```

Also update `buildExecJob` in `cmd/exec.go` with the same list.
Add the new builtin name to `validateBuiltinHostConflict` to prevent re-declaring it via `host_commands`.

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
            return b.execCommand(req, def)
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

git's `captureGitBinding` uses a trusted snapshot taken at registration time so that an agent
cannot tamper with the remote URL later. Add a capture only when the builtin references external
URLs or resources that must be protected from tampering.

---

## Step 6 — Tests

### 6a. Add matrix tests to `builtin_policy_test.go`

```go
// hook×oci AllowedOps must be empty.
func TestDefaultBuiltinPolicies_HookOciIsEmpty(t *testing.T) {
    policies := DefaultBuiltinPolicies(RoleHook, []string{"oci"})
    if len(policies["oci"].AllowedOps) != 0 {
        t.Errorf("hook×oci AllowedOps should be empty, got %v", policies["oci"].AllowedOps)
    }
}

// gate×oci must include {run}.
func TestDefaultBuiltinPolicies_GateOciHasRun(t *testing.T) {
    policies := DefaultBuiltinPolicies(RoleGate, []string{"oci"})
    if !policies["oci"].Allows(string(sandbox.OciOpRun)) {
        t.Error("gate×oci should allow run")
    }
}

// empty role must equal gate policy.
func TestDefaultBuiltinPolicies_EmptyRoleEqualsGate_Oci(t *testing.T) {
    gate := DefaultBuiltinPolicies(RoleGate, []string{"oci"})
    empty := DefaultBuiltinPolicies("", []string{"oci"})
    if !opsEqual(gate["oci"].AllowedOps, empty["oci"].AllowedOps) {
        t.Error("default oci policy should equal gate oci policy")
    }
}
```

### 6b. Create `oci_builtin_test.go`

Minimum scenarios to cover:

- A forbidden op is rejected (empty policy or policy that does not include the op)
- An allowed op passes through
- Missing or out-of-range cwd returns an error

Reference: test helpers in `internal/sandbox/git_builtin_test.go` (`initGitRepo`, `gateGitPolicies`, etc.).

---

## Step 7 — Security checklist

Check these items when the new builtin involves external communication or host resource access:

- [ ] **Least privilege**: does hook receive more permissions than necessary? Hook should be read-only / notify-only by default
- [ ] **Workspace isolation**: does the implementation respect existing workspace isolation (e.g. `entry.Context.AllowedProjectIDs`)?
- [ ] **Secret leakage**: do error messages or slog calls expose secrets or credentials?
- [ ] **Trusted snapshot**: can an agent tamper with external URLs or resource references? (see `captureGitBinding` pattern)
- [ ] **Minimal host access**: are host resources (files, network, processes) used to the minimum required?
- [ ] **Default role test**: is there an `EmptyRoleEqualsGate` equivalent test confirming the empty-role policy matches gate?

---

## Design principles

| Principle | Rationale |
|-----------|-----------|
| broker has no knowledge of role | Role decisions are centralized in `DefaultBuiltinPolicies`; broker consults only the policy table |
| policy is stamped at registration | Eliminates role evaluation at dispatch time; keeps broker simple |
| `default` = gate | Test compatibility. In production Role is always set, but empty role matches gate (most permissive) by convention |
| hook cannot fetch/push | Agents must not directly access host-side remotes (same principle for git and oci) |

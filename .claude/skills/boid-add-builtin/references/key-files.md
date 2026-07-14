# key-files: Main Files When Adding a boid Builtin

## Reference files

| File | Role |
|------|------|
| `internal/sandbox/protocol.go` | Defines `BuiltinPolicy`, `ExecRequest`, Op types and constants |
| `internal/orchestrator/policy.go` | `DefaultBuiltinPolicies` / `policyFor` — the policy table |
| `internal/sandbox/broker.go` | `Handle()` / `Register()` / `allowsBuiltinOp` helper, plus `handleBoidBuiltin` (example handler with policy check at the top) |
| `internal/sandbox/fetch_builtin.go` | Smaller single-op builtin — good reference for the minimum shape |
| `internal/orchestrator/spec_loader.go` | `validateBuiltinHostConflict` — prevents re-declaring builtin names in `host_commands` |
| `internal/orchestrator/planner.go` | Builtin name list in `PlanHook` |
| `internal/dispatcher/session_job.go` | Builtin name list in `BuildSessionJobSpec` (reused by `BuildExecJobSpec`) |

## Key types and functions for builtin implementation

### `internal/sandbox/protocol.go`

```go
// Entry point for all builtin requests
type ExecRequest struct {
    Command string
    Token   string
    Cwd     string
    Boid    *BoidRequest  // boid builtin
    Fetch   *FetchRequest // fetch builtin
    // Add new builtin fields here
}
```

### `internal/orchestrator/policy.go`

```go
// Orchestrator-owned, sandbox-agnostic policy type.
// AllowedOps is a sorted []string (not a map) for trivial comparison/serialisation.
type BuiltinPolicy struct {
    AllowedOps      []string
    AllowedCwdRoots []string
}
func (p BuiltinPolicy) Allows(op string) bool
func (p BuiltinPolicy) AllowsCwd(cwd string) bool
```

### `internal/sandbox/broker.go`

```go
// Check whether the token has a policy for the given builtin
func (e *tokenEntry) hasBuiltinPolicy(name string) bool

// Check whether the given op is permitted by the policy
func (e *tokenEntry) allowsBuiltinOp(name, op string) bool

// tokenEntry — holds the policy stamped at registration time
type tokenEntry struct {
    Context         TokenContext
    Commands        map[string]CommandDef
    BuiltinPolicies map[string]BuiltinPolicy
    // Add a new builtin's snapshot binding here only if needed. The retired `git`
    // builtin used `Git *GitBinding` to capture the remote URL at registration
    // time so an agent could not tamper with it later; today the git gateway
    // handles the same enforcement at the proxy layer.
}
```

### `internal/orchestrator/policy.go` (continued)

```go
// Entry point that returns a policy map given a role, builtin names, and policy context
func DefaultBuiltinPolicies(role Role, names []string, pctx PolicyContext) map[string]BuiltinPolicy

// Switch to add per-builtin policy functions
func policyFor(role Role, name string, pctx PolicyContext) BuiltinPolicy
```

## Rationale behind existing builtin policies

### boid builtin

**Role branching: none** — all roles share the same policy (`_ Role`).

Allowed ops (16 total):

| Op | Notes |
|----|-------|
| `job_done` | |
| `job_list` | |
| `job_show` | |
| `job_log` | |
| `action_send` | |
| `agent_stop` | |
| `task_create` | |
| `task_get` | |
| `task_update` | |
| `task_import` | |
| `task.reopen` | historically uses `.` separator (others use `_`) |
| `task_list` | |
| `task_notify` | |
| `task_answer` | |
| `task_ask` | |
| `task_delete` | |

### fetch builtin

**Role branching: none** — all roles share the same policy (`_ Role`).

Allowed op: `get` (broker-mediated HTTP GET only). No cwd restriction, since fetch performs no
local filesystem operations; the SSRF guard lives in the handler.

## Test file locations

| Test | File |
|------|------|
| policy matrix | `internal/orchestrator/policy_test.go` |
| fetch handler | `internal/sandbox/fetch_builtin_test.go` |
| broker helpers / token registration | `internal/sandbox/broker_test.go` |
| new builtin handler | `internal/sandbox/<name>_builtin_test.go` (create new) |

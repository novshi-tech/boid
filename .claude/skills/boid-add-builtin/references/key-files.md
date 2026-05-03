# key-files: Main Files When Adding a boid Builtin

## Reference files

| File | Role |
|------|------|
| `internal/sandbox/protocol.go` | Defines `BuiltinPolicy`, `ExecRequest`, Op types and constants |
| `internal/orchestrator/builtin_policy.go` | `DefaultBuiltinPolicies` / `policyFor` — the policy table |
| `internal/sandbox/broker.go` | `Handle()` / `Register()` / `allowsBuiltinOp` helper |
| `internal/sandbox/git_builtin.go` | Example handler with policy check at the top |
| `internal/orchestrator/spec_loader.go` | `validateBuiltinHostConflict` — prevents re-declaring builtin names in `host_commands` |
| `internal/orchestrator/planner.go` | Builtin name lists in `PlanHook` / `PlanGate` |
| `cmd/exec.go` | Builtin name list in `buildExecJob` |

## Key types and functions for builtin implementation

### `internal/sandbox/protocol.go`

```go
// Type that holds the allowed op set for a builtin
type BuiltinPolicy struct {
    AllowedOps map[string]struct{}
}
func (p BuiltinPolicy) Allows(op string) bool

// Entry point for all builtin requests
type ExecRequest struct {
    Command string
    Token   string
    Cwd     string
    Boid    *BoidRequest  // boid builtin
    Git     *GitRequest   // git builtin
    // Add new builtin fields here
}
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
    Git             *GitBinding  // snapshot for git
    // Add a new builtin's binding only if needed
}
```

### `internal/orchestrator/builtin_policy.go`

```go
// Entry point that returns a policy given a role and builtin name
func DefaultBuiltinPolicies(role Role, names []string) map[string]sandbox.BuiltinPolicy

// Switch to add per-builtin policy functions
func policyFor(role Role, name string) sandbox.BuiltinPolicy
```

## Rationale behind existing builtin policies

### boid builtin

| role | allowed ops |
|------|-------------|
| hook | `job_done`, `task_get` |
| gate (default) | `job_done`, `task_create`, `task_update`, `task_import` |

hook is restricted so agents cannot create or update tasks (read-only + completion notification only).

### git builtin

| role | allowed ops |
|------|-------------|
| hook | none (empty policy) |
| gate (default) | `fetch`, `push` |

Direct git operations via the broker from hook are forbidden. Agents must not access host-side remotes directly.

## Test file locations

| Test | File |
|------|------|
| policy matrix | `internal/orchestrator/builtin_policy_test.go` |
| git handler | `internal/sandbox/git_builtin_test.go` |
| new builtin handler | `internal/sandbox/<name>_builtin_test.go` (create new) |

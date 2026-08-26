# key-files: Main Files When Adding a boid Builtin

**2026-08-26 update (PR-3 of docs/plans/signal-ingest-detailed-design.md):** this
file previously covered only the "add a brand-new builtin command" path (the `oci`
example below). In practice nearly every real addition in this codebase is the
*other* path — adding a new **op** to the already-registered `boid` builtin
(`card_get`/`card_list`, `action_list`, `task_identity_link/unlink/resolve`,
`task_resolve_or_capture`, and most recently `signal_list`/`signal_ack`/
`signal_ingest`/`signal_cursor_get`) — and this file said nothing about it: it
never mentioned `internal/sandbox/boid_shim.go` (where a new `boid <cmd>
<subcommand>` is parsed) or `internal/server/boid_executor.go` (where a new op's
actual business logic lives — NOT `internal/sandbox/boid_executor.go`, which is
just a 13-line interface declaration), and it said nothing about the two
op-registry drift checks (`policy_ops.go` mirror + `broker_op_escape_test.go`'s
hard-fail manifest) that a new op must also satisfy. See SKILL.md's new "Adding a
new OP to the existing `boid` builtin" section for the corrected, common-case
checklist — the tables below now cover BOTH paths.

## Reference files — adding a new OP to the existing `boid` builtin (common case)

| File | Role |
|------|------|
| `internal/sandbox/protocol.go` | `BoidOp` constants + any new `BoidRequest` fields the op needs |
| `internal/orchestrator/policy_ops.go` | `OpBoid*` string constants — a byte-identical MIRROR of the `sandbox.BoidOp*` constants above. Required because `orchestrator` cannot import `sandbox` (layer direction); see the file's own doc comment |
| `internal/orchestrator/policy.go` | `boidPolicy()` — add the new `OpBoid*` constant to its `sortedOps(...)` call to grant it to every hook/exec job. **Do not add it here if the op is meant to be restricted to a narrower policy** (e.g. a future connector-only reduced policy) — see `signal_ingest`/`signal_cursor_get`'s own doc comment there for a worked exception |
| `internal/sandbox/boid_shim.go` | `parseBoidRequest`'s big command/subcommand switch, plus a new `parseBoid<Thing>(args []string) (*BoidRequest, error)` function — this is the CLI surface for `boid <command> <subcommand> [flags]` inside the sandbox |
| `internal/sandbox/broker.go` | `handleBoidBuiltin`'s per-op `switch boidReq.Op` — required-field validation and any broker-authoritative scoping (e.g. injecting `WorkspaceID`/`ProjectID` from the token, never trusting a caller-supplied value) BEFORE the request reaches the executor |
| `internal/server/boid_executor.go` | `boidBuiltinExecutor.ExecuteBoidBuiltin`'s per-op `switch req.Op` — the actual business logic (calling into whatever store/service backs the op). This is a DIFFERENT file from `internal/sandbox/boid_executor.go` (see above) |
| `internal/dispatcher/policy_translate_test.go` | `TestOpConstantsMirror` — add a `{orchestrator.OpBoid..., string(sandbox.BoidOp...)}` pair so the mirror added to `policy_ops.go` is actually checked for drift |
| `internal/orchestrator/policy_test.go` | `TestDefaultBuiltinPolicies_HookBoidOps`'s `wantOps` — a plain `len(a) != len(b)` comparison (`opsEqual`), so forgetting to add the new op here fails immediately, not silently |
| `internal/sandbox/broker_op_escape_test.go` | `opEscapeCoverage` — an AST-driven hard-fail manifest: it parses every `BoidOp` constant straight out of `protocol.go` and requires each one to have a named guard test (or an explicit `exempt` reason) registered here. **Forgetting this step fails the build with a clear message naming the missing op** — see the file's own top-of-file doc comment |

## Reference files — adding an entirely new builtin command (rare; the `oci` example below)

| File | Role |
|------|------|
| `internal/sandbox/protocol.go` | Defines `BuiltinPolicy`, `ExecRequest`, Op types and constants |
| `internal/orchestrator/policy.go` | `DefaultBuiltinPolicies` / `policyFor` — the policy table |
| `internal/sandbox/broker.go` | `Handle()` / `Register()` / `allowsBuiltinOp` helper, plus `handleBoidBuiltin` (example handler with policy check at the top) |
| `internal/sandbox/fetch_builtin.go` | Smaller single-op builtin — good reference for the minimum shape |
| `internal/orchestrator/spec_loader.go` | `validateBuiltinHostConflict` — prevents re-declaring builtin names in `host_commands` |
| `internal/orchestrator/planner.go` | Builtin name list in `PlanHook` |
| `internal/dispatcher/session_job.go` | Builtin name list in `BuildSessionJobSpec` (reused by `BuildExecJobSpec`) |

Note: `broker_op_escape_test.go`'s AST manifest is scoped specifically to the
`BoidOp` enum (see its own doc comment) — it does not automatically cover a brand
new builtin's own op enum (there is none yet, since only `boid` and the
single-op `fetch` exist today). If a new builtin grows its own multi-op enum the
way `boid` has, consider adding an equivalent escape-manifest test for it rather
than leaving that class of op unchecked.

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

**Do not hardcode a count here — it drifts fast** (this table went stale at
"16 total" while the real list grew to 32, then 34, across several PRs before
anyone caught it; that staleness is exactly why this file needed the 2026-08-26
update). `boidPolicy()` in `internal/orchestrator/policy.go` is the single
source of truth — read its `sortedOps(...)` call directly rather than trusting
a table like this one to be current. As of the `signal_list`/`signal_ack`
addition (docs/plans/signal-ingest-detailed-design.md PR-3, 2026-08-26) it
allows 34 ops, spanning job/task/action/project/card/task-identity/
action-list/signal groups — `job_done`, `job_list`, `job_show`, `job_log`,
`action_send`, `agent_stop`, `task_create`, `task_get`, `task_update`,
`task_import`, `task.reopen` (historically uses `.`, not `_`), `task_list`,
`task_notify`, `task_answer`, `task_ask`, `task_delete`, `task_current`,
`task_instructions`, `task_env`, `task_payload`, `task_attachments_list`,
`task_attachments_get`, `task_update_payload_patch`, `project_behaviors`,
`project_list`, `card_get`, `card_list`, `task_identity_link`,
`task_identity_unlink`, `task_identity_resolve`, `task_resolve_or_capture`,
`action_list`, `signal_list`, `signal_ack`.

**Not every declared `BoidOp` belongs in this list.** `signal_ingest` and
`signal_cursor_get` are declared in `protocol.go`/`policy_ops.go` (so the
mirror/escape-manifest checks cover them) but deliberately excluded from
`boidPolicy`'s `AllowedOps` — they're reserved for a narrower, connector-only
policy a later PR grants separately. See `boidPolicy`'s own doc comment in
`policy.go` for the live example. When adding a new op, default to adding it
here (the common case) — but check whether the op is actually meant to be
generally available before reflexively "completing the set".

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

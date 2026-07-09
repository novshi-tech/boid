# boid wiring-seam catalog

A list of cross-package "change one end and the other silently breaks" wiring paths. During
review, cross-reference the diff's changed files against this catalog and, for every seam
that hits, check its **two ends** and its **guard test**.

Never write line numbers (they rot). Reference by file + function / type name. Every entry
has the same shape:

- **Ends**: the places that must stay consistent (across packages)
- **Invariant**: the property both ends must uphold
- **Past break**: an actual regression, if any
- **Guard**: the test that protects this seam today
- **When you touch it**: what a reviewer must check when the diff touches this seam

## Contents

1. [binding two-tier wiring (hydrate → sandbox builder)](#1-binding-two-tier-wiring)
2. [builtin op ↔ escape guard / policy drift](#2-builtin-op--escape-guard)
3. [HarnessType propagation (JobSpec builder → registry.For)](#3-harnesstype-propagation)
4. [session jsonl persistence (env strip)](#4-session-jsonl-persistence)
5. [workspace allowed_domains (proxy)](#5-workspace-allowed_domains)
6. [brokered git remote snapshot](#6-brokered-git-remote-snapshot)
7. [embedded-skill bind (adapter.Bindings)](#7-embedded-skill-bind)
8. [host_commands CommandDef mirror (spec → broker gate)](#8-host_commands-commanddef-mirror)
9. [gitgateway RepoKey normalization](#9-gitgateway-repokey-normalization)

---

## 1. binding two-tier wiring

boid's bindings are wired in two tiers: **upstream (hydrate) → downstream (sandbox builder)**.

- **End A (upstream)**: `mergeBindMounts` in `internal/orchestrator/spec_loader.go`, and the
  `Meta.AdditionalBindings` returned by `GetProject`. It **merges** the workspace kit's
  `additional_bindings` into the project's bindings and returns the merged set. The API exit
  `ProjectAppService.GetProject` in `internal/api/project_service.go` must return the same merged
  set on re-fetch.
- **End B (downstream)**: `BuildSandboxSpec` in `internal/dispatcher/sandbox_builder.go`.
  It expands via `additionalBindingMounts` / `expandWorktreeBindings`. **Mounts** append
  **both** the harness bindings (`registry.For(...).Bindings()`) and the kit / additional
  bindings (additive). **PATH** (the `pathBindings` passed to `buildPATH`) takes the harness
  set **exclusively** when a harness is present — this is by design (a kit's executables reach
  PATH via a host_commands shim, not via `additional_bindings`). Mounts and PATH are
  intentionally asymmetric.
- **Invariant**: **mounts are additive, never an exclusive replacement** (env reflected but
  mounts dropped is the definitive symptom of a broken wire). Meanwhile the **PATH harness
  exclusivity is correct**, so don't mistake `pathBindings = harnessBindings` on its own for a
  regression. The regression is "additional_bindings dropped on the mount side", not the PATH
  asymmetry.
- **Past break**: `d464581` ("add codex / opencode adapter") branched the **mount** side on
  `len(harnessBindings) > 0` and appended only the harness bindings, silently dropping the kit
  `additional_bindings` (fixed to an unconditional additive append in PR #674, `4cd50c5`). Upstream
  was also returning a raw hydrate (fixed in PR #675, `33ac4cf`). An "equivalent to claude" claim +
  no test crossing the seam let it slip past the 1-turn smoke.
- **Guard**: downstream = `TestBuildSandboxSpec_ProfileInit_HarnessKeepsAdditionalBindings` /
  `ProfileDefault_...` in `sandbox_builder_test.go` (**only reproduces in the agent class** —
  exec/shell have empty harness bindings and don't enter `if len(harnessBindings) > 0`).
  Upstream = `TestGetWithWorkspace_AdditionalBindingsMerge` in `project_store_hydrate_test.go`.
  End-to-end = `TestBindingPassthrough_HydrateToSandboxSpec`.
- **When you touch it**: if you touch an adapter's `Bindings()`, the binding merge in
  `sandbox_builder.go`, or the hydrate in `spec_loader.go`, verify **both tiers** stay additive
  and that an agent-class test exists. Any "same as claude" claim must produce evidence that
  additional_bindings still flow.

## 2. builtin op ↔ escape guard

The core of boid's security model: the correspondence between builtin ops a sandboxed agent can
invoke and the escape path that permits them.

- **End A**: the op constants in `internal/orchestrator/policy_ops.go` (`OpBoidTaskCreate`, etc.).
  These are a **mirror** of the op constants in `internal/sandbox/protocol.go`; orchestrator can't
  import sandbox (layering runs the other way), so they're kept in lock-step via string literals.
- **End B**: the policy table (which JobKind permits which op) and the actual broker dispatch
  handling.
- **Invariant**: (1) the op constants on both sides match. (2) when you add a new builtin /
  docker-proxy op, a corresponding escape test (unit or e2e) is **paired with it**, or it is
  explicitly placed on an exemption list.
- **Guard**: constant drift = `internal/dispatcher/policy_translate_test.go` (the only layer that
  can see both sides). Permitted op set = `wantOps` in `internal/orchestrator/policy_test.go`.
- **When you touch it**: if you add or rename a builtin op, check that **both** `policy_ops.go`
  and `protocol.go` are updated, plus `wantOps` in `policy_test.go` and
  `policy_translate_test.go`, and that a corresponding escape/permission test exists. This is the
  spot where the update discipline on adding an op is enforced by a human, not a mechanism.
- Related: `.claude/skills/boid-add-builtin` (the add-a-builtin checklist)

## 3. HarnessType propagation

Whether the `HarnessType` each JobSpec builder sets propagates all the way through to resolving
the right adapter on the sandbox side.

- **End A (entry)**: `BuildSessionJobSpec` (`boid agent`) / `BuildExecJobSpec` (`boid exec`) in
  `internal/dispatcher/session_job.go`, and `PlanHook` (task hook) in
  `internal/orchestrator/planner.go`. Each sets `HarnessType`. The hook resolves it from the agent
  name via `harnessTypeForAgent` (`planner.go`).
- **End B (exit)**: `registry.For(HarnessType).Bindings()` in
  `internal/dispatcher/sandbox_builder.go`, and `registry.For(spec.HarnessType)` in
  `internal/sandbox/runner/runner_linux.go`.
- **Invariant**: exec is **forced to shell** (ignores the caller value), session is passthrough,
  hook goes via `harnessTypeForAgent`. An unknown agent name falls back to shell. An empty or
  wrong `HarnessType` means a runner-guard 127 exit, or a wrong adapter with missing bindings.
- **Past break**: in Phase 3-d, `BuildCommandJobSpec` (since removed) missed setting `HarnessType`
  → exec hit a runner-guard 127. Around the same time, the HarnessType branch in `sandbox_builder.go` lost
  KitRoots when the shell adapter (Bindings=nil) was selected.
- **Guard**: `internal/dispatcher/session_job_test.go` (the field contract of `BuildExecJobSpec` /
  `BuildSessionJobSpec`), `TestPlanHook_CarriesAdditionalBindings` and others in
  `internal/orchestrator/planner_test.go`, plus `TestBuildSandboxSpec_ShellHarnessKeepsKitRoots`
  (shell class).
- **When you touch it**: if you touch a JobSpec builder, `harnessTypeForAgent`, or `registry.For`,
  verify the HarnessType is right for all three entries (exec/session/hook) and that both the shell
  class and the agent class are in the test matrix (some regressions only show in the agent class).

## 4. session jsonl persistence

Whether Claude Code's session log (`~/.claude/projects/.../*.jsonl`) is correctly persisted by the
sandboxed claude.

- **End**: `adapter.Run()` in `internal/adapters/claude/run.go`. If `CLAUDE_CODE_CHILD_SESSION`
  leaks from the parent claude-code through the daemon into the sandboxed child claude, the child
  won't materialize the session.
- **Invariant**: `adapter.Run()` must **strip** `CLAUDE_CODE_CHILD_SESSION` and **inject**
  `FORCE_SESSION_PERSISTENCE=1`. Don't break those two when you touch env propagation. Relatedly,
  Claude CLI 2.1.181+ rejects starting as inner uid 0, so it's worked around with `IS_SANDBOX=1`.
- **Past break**: in Phase 3-b, the env leak meant the session jsonl wasn't persisted.
- **When you touch it**: if you touch the adapter's env construction, `Run()`, or the daemon →
  sandbox env handoff, verify the strip/inject above is preserved. Any "pass env through as-is"
  claim must confirm the exception for those two env vars.

## 5. workspace allowed_domains

Whether the egress allowlist is composed correctly as floor (global) + workspace (additive) and
reaches the per-workspace proxy.

- **End A**: `WorkspaceMeta.AllowedDomains` (`internal/orchestrator/workspace_meta*.go`).
- **End B**: `ProxyManager.GetOrCreate(workspaceID, allowed)` in
  `internal/sandbox/proxy_manager.go` (instantiated and driven from `internal/server`), and
  `resolveWorkspaceProxy` in `internal/dispatcher/runner.go`, which SetAllowed on every dispatch.
- **Invariant**: floor + workspace **additive** composition. The floor can't be removed. If
  resolution fails, dispatch is **not** blocked (fallback). Must be race-safe.
- **When you touch it**: if you touch allowed_domains, the proxy manager, or
  `resolveWorkspaceProxy`, verify the floor is preserved, the composition is additive (not a
  replacement), and dispatch doesn't stall on the fallback path.

## 6. brokered git remote snapshot

The binding snapshot that decides which remotes a brokered git push/fetch may reach.

- **End**: `internal/sandbox/git_builtin.go`. The binding's remote set is snapshotted **once** at
  token-registration time.
- **Invariant**: known remotes are resolved from the snapshot (no re-capture = the trusted-snapshot
  guarantee). Re-capture **only** when a remote is not in the snapshot. The log line
  `snapshot ready ... remotes=N` is the evidence.
- **Past break**: a remote added later (e.g. via `gh repo create`) isn't in the snapshot, so push is
  rejected "for just one project".
- **When you touch it**: if you touch the git builtin, the snapshot, or remote resolution, verify
  you haven't broken the trusted-snapshot guarantee (known remotes are not re-fetched) and that
  re-capture happens only on a miss.

## 7. embedded-skill bind

Whether the embedded skills appear at `~/.claude/skills/<name>` inside each harness's sandbox.

- **End**: each adapter's `Bindings()` (`internal/adapters/{claude,codex,opencode}/bindings.go`).
  Binds the host `~/.local/share/boid/skills/<name>` to `~/.claude/skills/<name>` inside the
  sandbox.
- **Invariant**: the bind path is aligned across all three adapters — claude / codex / opencode
  (`additionalBindingMounts` skips entries where `Source==Target`, so emit an empty Target).
- **When you touch it**: if you touch an adapter's `Bindings()`, verify the skills bind is
  preserved for all three harnesses. A claim that fixed just one harness ("fixed claude") should be
  suspected of collateral damage to the other two — this is exactly the regression mechanism of
  seam 1.
- Guard: `internal/adapters/claude/bindings_test.go` and the bindings tests of each adapter.

## 8. host_commands CommandDef mirror

Whether a host_commands policy field declared in YAML actually reaches the broker's
enforcement gate. Two mirror structs exist on purpose (orchestrator cannot be imported
by sandbox), so every new policy field must be threaded through each hop by hand.

- **End A (spec)**: `HostCommandSpec` / orchestrator `CommandDef` and `ToCommandDef` in
  `internal/orchestrator/spec_types.go` (transport shape).
- **End B (enforcement)**: sandbox `CommandDef` in `internal/sandbox/policy.go` and the
  shared pre-exec gate `gateHostCommand` in `internal/sandbox/broker.go` (used by both
  the non-streaming and the streaming path).
- **Hops in between**: the single type-conversion seam `toSandboxCommandDefs` in
  `internal/server/broker_adapter.go`, and the whole-struct copy in `ResolveHostCommands`
  (`internal/dispatcher/host_commands.go`) which passes fields through only as long as it
  stays a struct copy (`cd := def`).
- **Invariant**: a field added to `HostCommandSpec` must appear in **both** CommandDef
  mirrors, in `ToCommandDef`, and in `toSandboxCommandDefs`; enforcement must live in
  `gateHostCommand` so the streaming and non-streaming paths cannot drift apart.
- **Guard**: `TestToSandboxCommandDefs_FieldPassthrough` (`internal/server/broker_adapter_test.go`),
  `TestResolveHostCommands_RejectRulesPassthrough` (`internal/dispatcher/host_commands_test.go`),
  and the per-path enforcement tests in `internal/sandbox/broker_reject_test.go` /
  `broker_reject_streaming_test.go`.
- **When you touch it**: adding or removing a host_commands policy field means updating
  every hop above plus the passthrough tests; enforcement added to only one of the two
  exec paths (streaming vs non-streaming) is the classic one-ended break here. The
  agent-facing surface (`buildEnvironmentYAML` in `internal/dispatcher/sandbox_builder.go`)
  intentionally shows a **subset** (no path/env) — don't "fix" that asymmetry, but do keep
  reject rules visible to the agent.

## 9. gitgateway RepoKey normalization

Whether a repo identity resolves to the *same* `gitgateway.RepoKey` on both the register side
(dispatch-time allowlist construction) and the lookup side (an incoming gateway request), despite
the two sides starting from different string shapes (a captured `upstream_url` vs. a URL path
segment, either of which may or may not carry a `.git` suffix).

- **End A (register)**: `repoKeyFromUpstreamURL` in `internal/dispatcher/gitgateway_wire.go`,
  called from `Runner.buildGatewayRepos` for the self project, workspace peers, and workspace
  `extra_repos`. It splits a `host/owner/repo` slug (from `repoSlugFromOriginURL`) and always
  finishes with `gitgateway.NewRepoKey(host, owner, repo)` — never a raw
  `gitgateway.RepoKey(string(...))` conversion.
- **End B (lookup)**: `parsePath` + `route.repoKey()` in `internal/gitgateway/route.go`, invoked
  from `Server.ServeHTTP` for every incoming gateway request. `repoKey()` also always finishes
  with `gitgateway.NewRepoKey(r.host, r.owner, r.repo)`.
- **Invariant**: `NewRepoKey` is the *only* place that decides suffix normalization (it strips a
  trailing `.git` from the repo segment). Both ends must route through it — if either end ever
  starts building a `RepoKey` by any other means (string concatenation, a different
  suffix-stripping rule, case-folding a host differently), the two sides drift apart and
  `Registry.Authorize` silently 403s a request that should have been allowed (or worse, allows one
  that shouldn't be, if the drift happens to collide with a different repo's key).
- **Guard**: `TestServeHTTP_AcceptsBothGitSuffixForms` (`internal/gitgateway/server_test.go`)
  proves the lookup side accepts both suffix forms; `TestRepoKeyFromUpstreamURL_HTTPS` /
  `_SSH` (`internal/dispatcher/gitgateway_wire_test.go`) prove the register side normalizes both
  URL forms to the identical key `NewRepoKey` would produce from the same host/owner/repo.
  `TestDispatch_RegistersAndUnregistersGatewayToken` closes the loop end-to-end through the real
  `Dispatch` → `Registry.Register` → `Registry.Lookup` path.
- **When you touch it**: if you touch `repoKeyFromUpstreamURL`, `route.repoKey()`, or
  `NewRepoKey` itself, verify neither register nor lookup ever constructs a `RepoKey` by any path
  that bypasses `NewRepoKey`, and that a repo registered via one URL form (e.g. SSH) is reachable
  via a gateway path using the other form (e.g. HTTPS, with or without `.git`).

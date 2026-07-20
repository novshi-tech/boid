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
6. [embedded-skill bind (adapter.Bindings)](#6-embedded-skill-bind)
7. [host_commands CommandDef mirror (spec → broker gate)](#7-host_commands-commanddef-mirror)
8. [gitgateway RepoKey normalization](#8-gitgateway-repokey-normalization)
9. [sandbox-clone declaration path](#9-sandbox-clone-declaration-path)
10. [exec stdin-forward opt-in](#10-exec-stdin-forward-opt-in)
11. [gitgateway SecretResolver namespace threading](#11-gitgateway-secretresolver-namespace-threading)
12. [KitMeta.KitRoot ↔ sandbox_builder KitRoots mount](#12-kitmetakitroot--sandbox_builder-kitroots-mount)
13. [task-context RPC ↔ dispatch-time context files](#13-task-context-rpc--dispatch-time-context-files)
14. [shim command-name resolution (BOID_HOST_COMMAND_NAMES ↔ broker Commands key)](#14-shim-command-name-resolution)
15. [attachments RPC ↔ dispatch-time attachments bind](#15-attachments-rpc--dispatch-time-attachments-bind)
16. [adapter-issued task-context RPC (claude readSessionsFromRPC ↔ sandbox env)](#16-adapter-issued-task-context-rpc)

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

## 6. embedded-skill bind

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

## 7. host_commands CommandDef mirror

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

## 8. gitgateway RepoKey normalization

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

## 9. sandbox-clone declaration path

Whether a task/hook/session/exec job's branch declaration actually reaches the runner's clone
sequence, and whether the mount side stays in lockstep with the declaration side. Added by PR5
(`docs/plans/git-gateway-cutover.md`), engaged for real dispatch by PR6 (cutover).

- **End A (declare)**: `orchestrator.BuildCloneDeclaration` (`internal/orchestrator/head_branch.go`,
  called from `PlanHook` in `planner.go`) for task/hook jobs, and
  `dispatcher.buildSessionCloneDeclaration` (`internal/dispatcher/session_job.go`) for
  session/exec jobs. Both populate `orchestrator.Visibility.Clone` (`*CloneDeclaration`) with
  `Branch` / `BaseBranch` / `CheckoutOnly` / `BaseBranchForkPoint` — a pure declaration, no git
  executed yet. **docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11)** removed the
  per-task `ForkPoint` field entirely (along with `ComputeHeadBranch` / `ComputeForkPoint` and the
  `parent *Task` argument `BuildCloneDeclaration` used to take): `CheckoutOnly` is now
  unconditionally `true` and `Branch` always equals `BaseBranch`, for every task kind. Don't
  confuse the retired per-task `ForkPoint` with `BaseBranchForkPoint` (the unrelated case-3
  "`base_branch` doesn't exist on origin yet" start point, which is untouched — see End D).
- **End B (translate)**: `dispatcher.buildCloneSpec` (`internal/dispatcher/sandbox_builder.go`)
  converts the declaration + `Runner`-resolved facts (`rt.GatewayCloneURL`) into
  `sandbox.CloneSpec`, which `BuildSandboxSpec` attaches to `sandbox.Spec.Clone`.
- **End C (mount)**: `dispatcher.cloneMounts` (same file) — a **parallel, independently-gated**
  wire that must agree with End B on the same `spec.Visibility.Clone != nil` condition. It builds
  the RO `.git` reference-dir binds (self + workspace peers, at `sandboxCloneReferenceDir` /
  `sandboxClonePeerReferenceDirFmt`) and the `/workspace` bind from `rt.CloneWorkspaceDir`
  (`Runner.Dispatch` pre-allocates `<RuntimesDir>/<job.ID>/workspace` and mkdir's it before
  `BuildSandboxSpec` runs). `BuildSandboxSpec`'s project-visibility switch must also route to the
  clone-only tmpfs-HOME branch (skipping `projectVisibilityMounts`) whenever `Clone != nil` — see
  the PR5 Opus review's double-mount concern.
- **End D (execute)**: `runner.performClone` (`internal/sandbox/runner/clone.go`), invoked from
  `RunInnerChild` (`internal/sandbox/runner/runner_linux.go`) only when `spec.Clone.Enabled`. Clones
  from `cs.URL` (the gateway clone URL, carrying a live job token — redacted via
  `redactCloneURLToken` before it reaches any error string or `runner-state.json`), optionally with
  `--reference cs.ReferenceDir`, into `cs.TargetDir` (`/workspace`), then resolves `Branch`/
  `BaseBranch` against the fresh clone via `resolveCloneBranch`. `CheckoutOnly` is now the only
  live branch (`checkout -B Branch <resolved BaseBranch ref>`); the `CheckoutOnly == false` path
  is a defensive dead-end that returns an error (per-task fork-branch resolution — `resolveCloneRef`
  — was deleted in Phase 1). `BaseBranchForkPoint`'s `resolveCloneForkStart` (case 3: `BaseBranch`
  missing from both origin and locally) is untouched and still live.
- **Invariant**: (1) End A's `CheckoutOnly` is unconditionally `true` for every task as of Phase 1,
  and as of Phase 2 (branch-policy-simplification, 2026-07-16) the `Task.Worktree` /
  `ProjectMeta.Worktree` / `BehaviorResolution.Worktree` fields no longer exist at all. The
  `tasks.worktree` DB column is left in place (NOT NULL DEFAULT FALSE, migration `0007`) for BC —
  SQL INSERT/UPDATE/SELECT no longer reference it, so it writes the column default and is invisible
  to callers. If a future change wants to reintroduce a per-task "worktree" concept, don't
  reintroduce silent-write-no-read: pick one contract (drop the DB column via migration, or wire the
  field back through the resolver) rather than the previous half-wired state. (2) End B/C's
  `spec.Visibility.Clone != nil` gate must be checked identically
  everywhere it appears (`resolveWorkDir`, the mount switch, `cloneMounts`, `buildCloneSpec`) — a
  mismatch between any two of these is exactly the double-mount / no-mount class of bug. (3) End D
  never gets a real git binary path threaded to it anymore post-cutover (`CloneSpec.RealGitBin` is
  left unset) — the sandbox's own `git` on `$PATH` is the real binary now that the git-shim overlay
  is retired (git gateway cutover PR6/PR8); don't reintroduce a bind for this.
- **Past break**: none yet (PR5 was inert; PR6 is this seam's first real-dispatch exercise) — this
  entry exists so the *next* touch has a map, not so it documents a regression already found.
- **Guard**: `TestCloneMounts_*` / `TestBuildCloneSpec_*` / `TestResolveWorkDir_CloneEnabled_*` /
  `TestBuildSandboxSpec_CloneEnabled_SkipsProjectVisibilityMounts` (`internal/dispatcher/
  sandbox_builder_test.go`), `TestPerformClone_*` (`internal/sandbox/runner/clone_test.go`,
  `clone_e2e_test.go`), `TestBuildCloneDeclaration_*` (`internal/orchestrator/head_branch_test.go`).
- **When you touch it**: if you touch any of the four ends, verify the other three still agree —
  in particular, a change to `Visibility.Clone`'s shape (End A) must be reflected in both
  `buildCloneSpec` (End B) and `performClone`'s resolution logic (End D), and a change to the mount
  layout (End C) must not reintroduce a host `ProjectDir`/`WorktreeDir` bind for a clone-mode job.

## 10. exec stdin-forward opt-in

Whether the non-interactive (no-PTY) runtime transport allocates a live stdin-forwarding pipe only
for `boid exec`, never for a hook job. Added by PR #735 (git gateway cutover's exec-via-Dispatch).

- **End A (decide)**: `Runner.launchSandbox` in `internal/dispatcher/runner.go` sets
  `RuntimeStartSpec.StdinForward: job.Role == string(orchestrator.JobKindExec)` when calling
  `r.Runtime.Start`. This is the sole place that decides whether a dispatched job gets a live
  stdin pipe.
- **End B (act)**: `LocalRuntime.Start`'s non-interactive branch in
  `internal/dispatcher/runtime_local_linux.go` only opens the `stdinReader`/`stdinWriter` pipe pair
  when `spec.StdinForward` is true; otherwise `cmd.Stdin` is left unset (Go routes it to the null
  device). `localRuntimeSession.writeStdin` / `closeStdin` (same file) are the Attach-side write/EOF
  path that only has an effect when that pipe exists.
- **Invariant**: a non-interactive `JobKindExec` job **always** gets a live stdin pipe (so
  `echo hi | boid exec cat` reaches the child); every other non-interactive job (hook) **never**
  does — a hook script's `read` on stdin must keep observing an immediate EOF, the pre-existing
  contract. Interactive (PTY) jobs are unaffected either way (`StdinForward` is ignored when
  `Interactive` is true — the PTY master already carries stdin).
- **Past break**: none yet — this seam was introduced whole by PR #735, not discovered as a
  regression in an existing one.
- **Guard**: End A = `TestDispatch_ExecKindNonInteractive_SetsStdinForward` /
  `TestDispatch_HookKindNonInteractive_LeavesStdinForwardFalse`
  (`internal/dispatcher/runner_dispatch_test.go`). End B =
  `TestLocalRuntimeStdinForward_DeliversPipedInput` /
  `TestLocalRuntimeNonInteractiveWithoutStdinForward_DiscardsInput`
  (`internal/dispatcher/runtime_local_linux_test.go`).
- **When you touch it**: if you touch `launchSandbox`'s `RuntimeStartSpec` construction, add a new
  `JobKind`, or touch the non-interactive branch of `LocalRuntime.Start`, verify the two-sided
  contract still holds for **both** kinds in the same test run — a fix verified only against exec
  (or only against hook) is exactly the shape of break this seam exists to catch.

## 11. gitgateway SecretResolver namespace threading

Whether a workspace-scoped PAT namespace, chosen at dispatch time, actually reaches the
`SecretResolver` call that resolves the upstream Basic-auth token — namespace propagation
across register → store → recover → resolve (four ends, three hops between them), where
any hop that drops the namespace silently collapses every workspace back onto the
`"default"` secret namespace. Added by post-cutover 改善 §1 (workspace-scoped PAT
namespace).

- **End A (register)**: `Runner.registerGatewayToken` in `internal/dispatcher/gitgateway_wire.go`
  calls `r.GitGateway.Register(repos, spec.SecretNamespace)` — `spec.SecretNamespace` is already
  hydrated to the workspace ID upstream by `orchestrator.ProjectStore.GetWithWorkspace` (a
  pre-existing seam, unchanged here).
- **End B (store)**: `gitgateway.Registry.Register` / `RegisterToken` in
  `internal/gitgateway/registry.go` persist the namespace on `Entry.Namespace` alongside `Repos`.
- **End C (recover)**: `Server.ServeHTTP` in `internal/gitgateway/server.go` — after
  `Registry.Authorize` confirms the token, a second `Registry.Lookup(rt.token)` recovers
  `Entry.Namespace` (`Authorize`'s bool-returning signature does not expose it) and stashes it on
  the request-scoped `routeInfo`, which the `ReverseProxy.Rewrite` hook reads back to call
  `CredentialProvider.Inject(pr.Out, info.host, info.namespace)`.
- **End D (resolve)**: `gitgateway.SecretResolver` (`func(namespace, key string) (string, error)`,
  `internal/gitgateway/credentials.go`) — the closure built in `internal/server/wire.go`
  (`gwResolver`) passes `namespace` straight through to `secretStore.Get(namespace, key)`, which
  itself normalizes `""` to `"default"` (`dispatcher.SecretStore.normalizeNamespace`) — so an
  empty namespace (a workspace-unlinked project) still resolves against the pre-namespacing
  `"default"` secret namespace unchanged.
- **Invariant**: the namespace a token was registered with (End A/B) is exactly the namespace
  `Inject` resolves credentials against for every request authorized under that token (End C/D) —
  no hop may substitute, drop, or hardcode a different namespace (in particular, don't
  reintroduce a hardcoded `"default"` in the `gwResolver` closure the way the pre-fix code did).
- **Guard**: `TestRegistryRegisterAndLookupPreserveNamespace` / `_EmptyNamespacePreserved`
  (`internal/gitgateway/registry_test.go`, End B), `TestCredentialProviderInjectNamespaceRoutesToDifferentSecret`
  (`internal/gitgateway/credentials_test.go`, End D in isolation),
  `TestServeHTTP_RoutesCredentialsByTokenNamespace` (`internal/gitgateway/server_test.go`) closes
  End B→C→D end-to-end through a real `Registry` + `Server`, and
  `TestDispatch_RegistersGatewayTokenWithSecretNamespace` (`internal/dispatcher/gitgateway_wire_test.go`)
  closes End A→B through a real `Dispatch`.
- **When you touch it**: if you touch `registerGatewayToken`, `Registry.Register`/`RegisterToken`,
  `Server.ServeHTTP`'s post-`Authorize` block, or the `gwResolver` closure in
  `internal/server/wire.go`, verify a token registered under namespace X still resolves
  credentials under namespace X — a change to any one hop without updating the others
  reintroduces the "every workspace shares one PAT" bug this seam exists to prevent.

## 12. KitMeta.KitRoot ↔ sandbox_builder KitRoots mount

Whether a kit's on-disk root directory, collected while merging kit metadata into a
task behavior, actually ends up bind-mounted into the sandbox for jobs that still rely on the
legacy "expose the whole kit directory tree" binding path (shell-adapter jobs that predate
adapter-driven `Bindings()`).

- **End A (collect)**: `ReadKitMeta` (`internal/orchestrator/spec_loader.go`) sets
  `KitMeta.KitRoot` to the kit's directory. `MergeKitMetaIntoBehavior`
  (`internal/orchestrator/spec_loader.go`) dedupes and appends each kit's `KitRoot` onto
  `TaskBehavior.KitRoots`.
- **End B (relay)**: `DispatchPlanner.PlanHook` (`internal/orchestrator/planner.go`) copies
  `behavior.KitRoots` straight into `JobSpec.Visibility.KitRoots`.
- **End C (mount)**: `BuildSandboxSpec` (`internal/dispatcher/sandbox_builder.go`) iterates
  `spec.Visibility.KitRoots` and emits a read-only `sandbox.Mount{Source: kitRoot, Target:
  kitRoot}` for each — this is on top of, not instead of, the harness/kit `additional_bindings`
  mounts (see seam #1).
- **Invariant**: every kit root collected at End A is still present in `Visibility.KitRoots` by
  the time End C builds mounts, for **every** JobKind that reaches `PlanHook` (not just the
  agent-class path that seam #1's guard covers) — this is the one binding surface that still
  works when a job has no `HarnessAdapter.Bindings()` at all (shell adapter). Consumer example:
  PR2a (script-hook-removal) uses this path to distribute the `docker-proxy-test.sh` fixture
  read-only into e2e sandboxes via a kit root.
- **Guard**: End A = `TestMergeKitMetaIntoBehavior` (`internal/orchestrator/spec_loader_test.go`,
  asserts `KitRoots == ["/kit"]` after merge). End B = `TestPlanHook_SetsKitRootsFromBehavior`
  (`internal/orchestrator/planner_test.go`). End C =
  `TestBuildSandboxSpec_KitRootsAreBound` / `TestBuildSandboxSpec_ShellHarnessKeepsKitRoots`
  (`internal/dispatcher/sandbox_builder_test.go` — the latter specifically covers the
  no-`Bindings()` shell-adapter case this seam exists for).
- **When you touch it**: if you touch `MergeKitMetaIntoBehavior`, the `Visibility.KitRoots`
  assignment in `PlanHook`, or the kit-root mount loop in `BuildSandboxSpec`, verify a kit root
  set at End A still lands as a mount at End C — dropping any hop silently removes kit content
  from sandboxes that have no adapter-driven `Bindings()` to fall back on.

## 13. task-context RPC ↔ dispatch-time context files

Whether the new Phase 5b task-context broker RPCs (docs/plans/phase5-shim-and-task-context.md)
serve the same data the pre-existing dispatch-time context files
(`$HOME/.boid/context/{task,instructions,environment,payload}.{yaml,json}`) already write into
every sandbox — the two paths run in parallel from PR1 through PR5, so any drift is a silent
inconsistency between "what the agent reads at boot" and "what the agent reads if it calls the
new CLI later in the same job". The scoping key differs by RPC and getting it wrong is the
actual failure mode this seam has already produced once (see **Past break**):
`boid task current` is TaskID-scoped (safe — re-derives live from the task row, no per-job
ambiguity); `boid task instructions` / `env` / `payload` are all **JobID-scoped**, because their
source data is job-scoped, not task-scoped.

- **End A (file)**: `contextFiles` / `buildEnvironmentYAML` in `internal/dispatcher/sandbox_builder.go`.
  `instructions.yaml` is written **iff** `JobSpec.Instruction != nil` (this job's own routed
  instruction — `orchestrator.DispatchPlanner.PlanHook`'s `selectInstruction`, filtered by *this
  hook's* declared agent). `environment.yaml`'s `host_commands` is fed by
  `EnvironmentInput.HostCommands` (= `spec.HostCommands`, the **short-name-keyed** map — not
  `SandboxRuntimeInfo.ResolvedHostCommands`, which is absolute-host-path-keyed shim/broker plumbing
  only) via the shared `convertHostCommands` helper; `network.allowed_domains` comes from
  `SandboxRuntimeInfo.AllowedDomains` (= the `allowedDomains` local in `Runner.Dispatch`).
  `payload.json`/`.yaml` is `JobSpec.PrimaryInput` (already trait-filtered by
  `orchestrator.FilterPayloadByTraits` at plan time, per the firing hook's declared
  `Traits.Consumes`).
- **End B (RPC)**: `Runner.trackJobContext` (`internal/dispatcher/job_context.go`), called at the
  same point in `Runner.Dispatch` right after `resolveWorkspaceProxy`, builds a
  `JobContextSnapshot{Instructions: routedInstructionSlice(spec.Instruction), Env:
  BuildWorkspaceEnvView(allowedDomains, spec.HostCommands), Payload: spec.PrimaryInput}` — every
  field sourced from the **same** JobSpec values End A uses for *this exact job*, never re-derived
  from the task row. `boid task current` instead re-derives live from the task row
  (`orchestrator.SnapshotTask`) since that data has no job-scoped filtering dependency — see
  `internal/api/task_context.go`'s package doc comment for why only `current` gets that treatment.
- **End C (serve)**: `boidBuiltinExecutor.ExecuteBoidBuiltin`'s `BoidOpTaskInstructions` /
  `BoidOpTaskEnv` / `BoidOpTaskPayload` cases (`internal/server/boid_executor.go`) read back via
  the `jobContextProvider` interface, which `*dispatcher.Runner` satisfies structurally — wired in
  `internal/server/wire.go`'s `newBoidBuiltinExecutor(..., runner)` call using the **same**
  `runner` variable `Dispatch` runs on, not a separate instance. `internal/sandbox/broker.go`
  authorizes these three ops by strict `JobID` equality against the token's own context (never
  `TaskID`) — `BoidOpTaskCurrent` alone is authorized by `TaskID`.
- **Invariant**: End A and End B must derive from the identical per-job JobSpec values
  (`spec.Instruction`, `spec.HostCommands`, `spec.PrimaryInput`, `allowedDomains`) — a future
  refactor that re-derives any of them from the task row instead of the job's own JobSpec silently
  breaks parity even though both sides still "work" independently, because a task can have
  **multiple concurrent/sequential jobs whose routed instructions differ** (see Past break).
  `JobContextSnapshot` must not outlive its job: `Runner.UnregisterJob` must clear it (mirrors the
  broker token's own lifecycle).
- **Past break**: caught in review before merge, twice, on the same PR (#797):
  1. The first `Env` implementation used `resolvedHostCommands` (absolute-path-keyed) instead of
     `spec.HostCommands`, which `TestDispatch_TracksJobContext_EnvAndPayload` caught immediately
     (host command `Name` came back as `/usr/bin/gh` instead of `gh`).
  2. The first `boid task instructions` implementation derived from the task row
     (`orchestrator.CurrentInstructions`, filtering by the *active/last* instruction history entry)
     instead of the job's own `JobSpec.Instruction`. codex review caught this: `orchestrator.Evaluator`
     fires an agent-kind hook for **every** agent appearing anywhere in the instruction history
     (`extractInstructionAgents`), not just the active entry, so a task with history
     `[claude-code, codex]` dispatches both a claude-code hook and a codex hook in the same round —
     but `selectInstruction`/`FilterInstructions` only route the *last* entry, so only one of the two
     jobs gets a non-nil `Instruction` (and only one gets an `instructions.yaml` file). The task-row
     derivation had no way to tell the two jobs apart and would hand the wrong job the other agent's
     instruction the moment the 5b-6 cutover retired the file (which never had this bug, since it's
     keyed by the job's own `JobSpec.Instruction` from the start). Fixed by moving `Instructions` into
     `JobContextSnapshot` (JobID-scoped, same pattern as `Env`/`Payload`) before merge — see
     `orchestrator.CurrentInstructions`'s doc comment for what it's safe (and unsafe) to use for now.
- **Guard**: End A unchanged = the pre-existing `TestBuildEnvironmentYAML_*` suite in
  `sandbox_builder_test.go` (still green after the `WorkspaceEnvHostCommand` rename, proving the
  YAML tags/output didn't move) plus `TestPlanHook_Instruction_MatchingAgent` /
  `_NonMatchingAgent_ReturnsNil` (`internal/orchestrator/planner_test.go` — the latter is the
  root-cause case: a hook whose agent doesn't match the active history entry gets `Instruction ==
  nil` even though the evaluator fired it). End B =
  `TestDispatch_TracksJobContext_Instructions_MatchesJobSpec` /
  `_NilJobSpecInstructionYieldsEmpty`, `_EnvAndPayload`, `_NilPrimaryInput`,
  `TestUnregisterJob_RemovesJobContext` (`internal/dispatcher/runner_job_context_test.go`). End C =
  the `TestBoidBuiltinExecutor_Task{Instructions,Env,Payload,Current}_*` suite
  (`internal/server/boid_executor_task_context_test.go`), plus two real-`*dispatcher.Runner` wiring
  tests in `internal/server/boid_executor_task_context_wiring_test.go`:
  `TestBoidBuiltinExecutor_TaskEnvAndPayload_RealRunnerWiring` (env/payload, single job, plus the
  post-`UnregisterJob` failure case) and
  `TestBoidBuiltinExecutor_TaskInstructions_RealRunnerWiring_NoCrossJobLeak` (dispatches **two**
  real jobs sharing a simulated instruction history and asserts each job's `boid task instructions`
  call returns only its own data — the specific shape of the second Past break, closed at the
  layer a stub-only `jobContextProvider` test suite cannot reach). Broker-level authorization
  (id-equality against the token's own `TaskID`/`JobID`, `TaskInstructions` on `JobID` specifically)
  is covered separately by `internal/sandbox/broker_task_context_test.go`.
- **When you touch it**: if you touch `contextFiles`/`buildEnvironmentYAML`, `selectInstruction`/
  `Evaluator.Evaluate`, `Runner.Dispatch`'s `trackJobContext` call, or
  `jobContextProvider`/`newBoidBuiltinExecutor`'s wiring in `wire.go`, verify End A and End B still
  read from the same **per-job** JobSpec values (never the task row, for anything but
  `BoidOpTaskCurrent`) and that the real-Runner wiring tests still exercise the exact `runner`
  instance `wire.go` threads through. Any change that makes a task-context RPC read from the task
  row should immediately raise the question this seam exists to ask: "can two jobs from this same
  task disagree about this value?" — if yes, it must be JobID-scoped. The 5b-6 cutover PR
  (`docs/plans/phase5-shim-and-task-context.md`), which retires End A's file materialization, is
  the point where this seam collapses to just End B/C — update this entry then.

## 14. shim command-name resolution

Whether a shim invocation inside the sandbox identifies itself to the broker under the same
key the broker's `Commands` map is registered with. As of 5a-2
(`docs/plans/phase5-shim-and-task-context.md`) the shim sends the **declared short name**
(`host_commands.<name>`), not the absolute bind-mount path — but the shim only ever sees its
own bind-mount path (`os.Executable()`) and argv0, so for an aliased command
(`host_commands.<name>.path` pointing at a file whose basename differs from `name`, e.g.
`run-e2e` → `e2e/run.sh`) recovering the declared name requires a side-channel the dispatcher
must inject.

- **End A (dispatcher, inject)**: `buildHostCommandNamesEnv` in
  `internal/dispatcher/sandbox_builder.go`, fed by `rt.ResolvedHostCommands` (the **byPath**
  view of `dispatcher.ResolveHostCommands` — the exact same map `hostCommandMounts` binds the
  shim at). Sets `env[sandbox.HostCommandNamesEnv]` to a JSON `{bind-mount path: declared
  name}` map. Skipped entirely (env unset) when there are no host commands.
- **End B (shim, resolve)**: `sandbox.ResolveShimCommandName` (`internal/sandbox/shim.go`),
  called once per invocation from `main.go`'s `shimMain`. Looks up
  `shimBinaryPath(argv0)` (its own bind-mount path) in the `BOID_HOST_COMMAND_NAMES` map;
  falls back to `CommandFromArgv0(argv0)` (the file's basename) when the env var is
  unset/malformed/has no entry — which is correct whenever no alias is declared. The **same**
  resolved name feeds both `EarlyRejectFromEnv` (the shim-side fast-path reject check) and
  `ShimExec`'s `ExecRequest.Command` — they must never resolve to different names for the same
  invocation.
- **End C (broker, authorize)**: `entry.Commands` in `internal/sandbox/broker.go`, registered
  under the **byName** view (`dispatcher.CommandBroker.RegisterCommands`'s short-name-keyed
  input — see seam #7's sibling wiring). `lookupCommand` still carries a Path-match fallback
  for pre-5a-2 callers (kept intentionally per the 5a plan until the 5a-3 cutover; do not
  remove it as part of a 5a-2-shaped change).
- **Invariant**: for every `host_commands` entry, `buildHostCommandNamesEnv`'s map value at
  key `absPath` must equal the same `def.Name` the broker's `Commands` map is keyed by at End
  C — both ultimately derive from the single `ResolveHostCommands` call's `out` map
  (`byName[def.Name] == byPath[absPath]`, see `ResolveHostCommands`'s own doc comment), so a
  future refactor that lets End A and End C diverge onto two different resolved-command maps
  would silently break every aliased `host_commands` entry while leaving non-aliased ones
  (where basename already equals the declared name) looking fine.
- **Guard**: End A = `TestBuildSandboxSpec_HostCommandNamesEnv_MapsAliasedPathToDeclaredName`
  / `_AbsentWhenNoHostCommands` (`internal/dispatcher/sandbox_builder_test.go`). End B =
  `TestResolveShimCommandName_*` (`internal/sandbox/shim_command_name_test.go`) — in particular
  `_AliasedPathResolvesToDeclaredName`, which pins the exact bug this seam exists to prevent.
  End C (pre-existing) = `TestBroker_ShortNameKeyedCommand_*` in
  `internal/sandbox/broker_test.go`, plus `TestBroker_ShortNameKeyedCommand_AliasDirectMatch`
  added alongside this seam to pin the post-5a-2 direct-match case specifically (no Path-scan
  fallback needed once the shim sends the resolved short name). Full end-to-end (real sandbox,
  real shim binary, aliased `host_commands` entry) is covered by
  `e2e/scenarios/host-command-smoke`'s `alias-echo` command
  (`e2e/fixtures/kits/host-ops/kit.yaml`, invoked in the sandbox as `echo-target` — the file's
  actual basename, never the declared name).
- **When you touch it**: if you touch `hostCommandMounts`, `buildHostCommandNamesEnv`,
  `ResolveShimCommandName`, `ResolveHostCommands`'s byPath/byName split, or `lookupCommand`,
  verify a request through an **aliased** `host_commands.<name>.path` entry still resolves —
  the non-aliased case (basename == declared name) passes even when the alias-specific wiring
  is broken, so it is not a sufficient test on its own. 5a-3 (fixed-directory shim placement)
  is expected to make every shim's bind-mount basename equal its declared name by construction
  (symlinks named after the declared command), at which point `BOID_HOST_COMMAND_NAMES`
  becomes a pure defense-in-depth fallback rather than the primary resolution path — update
  this entry then.

## 15. attachments RPC ↔ dispatch-time attachments bind

Whether the Phase 5b PR2 attachments RPCs (`boid task attachments list` / `get <name>`,
docs/plans/phase5-shim-and-task-context.md) read from the identical on-disk directory the
pre-existing dispatch-time RO bind (`~/.boid/attachments`) exposes inside the sandbox — the two
paths run in parallel from PR2 through PR6, so any drift is a silent inconsistency between "what
the agent sees under `~/.boid/attachments`" and "what `boid task attachments get` returns for the
same name". Unlike seam #13 (task-context RPCs, which have per-job scoping subtlety), this seam's
risk is narrower in one dimension (attachments are TaskID-scoped only, no per-job ambiguity) but
wider in another: **three independent path-construction call sites** must agree on the same
directory — as of PR #798, all three now individually validate `taskID`, but two of them do it via
genuinely *different* (duplicated, not shared) code, which is its own drift risk (see Guard).

- **End A (bind)**: `internal/dispatcher/sandbox_builder.go`'s per-task attachments mount gates
  `spec.TaskID` through `isCanonicalTaskIDComponent` (`internal/dispatcher/attachments_path.go`)
  before building `attachSrc := filepath.Join(rt.AttachmentsRoot, "tasks", spec.TaskID,
  "attachments")` — a non-canonical `taskID` skips the mount entirely (fail-closed, the same
  "just omit it" behavior the pre-existing empty-`AttachmentsRoot`/empty-`taskID` cases already
  used). This is a **duplicate, not a call** to `api.isCanonicalPathComponent`: internal/api already
  imports internal/dispatcher (`job_log_sse.go`/`workspace_homes.go`/`ws_attach.go`), so the reverse
  import (dispatcher → api) would be a cycle — `attachments_path.go`'s doc comment explains this and
  points back here. `rt.AttachmentsRoot` is threaded from `dispatcher.RunnerConfig.AttachmentsRoot`,
  set in `wire.go` to `dataHomeFor(cfg)` — the same value End B/End C use.
- **End B (write path)**: `EnsureAttachmentsDir`/`SaveMultipartAttachments`
  (`internal/api/attachments.go`, called from `web.go`'s upload handlers) resolve the directory via
  `AttachmentsRootForTask(dataHome, taskID)`, which rejects a non-canonical `taskID` via
  `api.isCanonicalPathComponent`.
- **End C (RPC read path)**: `api.ListAttachments` / `api.ReadAttachment`
  (`internal/api/attachments.go`), called from `boidBuiltinExecutor`'s
  `BoidOpTaskAttachmentsList`/`Get` cases (`internal/server/boid_executor.go`), resolve through the
  same `AttachmentsRootForTask` as End B. The executor's `attachmentsRoot` field is threaded in
  `wire.go`'s `newBoidBuiltinExecutor(..., dataHomeFor(cfg))` call — the identical `dataHomeFor(cfg)`
  expression End A's `AttachmentsRoot:` field and End B's callers use.
- **End D (authorization)**: `internal/sandbox/broker.go`'s `BoidOpTaskAttachmentsList`/`Get` case
  authorizes by strict `TaskID` *string equality* against the token's own context (same pattern as
  `BoidOpTaskCurrent`) — it never resolves a filesystem path, so it cannot itself catch a
  traversal-shaped `TaskID`; that is End A/B/C's job.
- **Invariant**: all three path-construction call sites (End A/B/C) must resolve to the identical
  directory for a given `(dataHome, taskID)` pair, AND every one of them must reject the same set of
  non-canonical `taskID` values (empty, containing a path separator, or the literal `.`/`..`) before
  ever constructing a path — a `taskID` that passes End D's raw string-equality check must never be
  allowed to resolve, via `filepath.Join`'s automatic `..`-collapsing, to a *different* task's
  directory (see Past break for the concrete exploit this produces when a guard is missing). Because
  End A's guard (`isCanonicalTaskIDComponent`) and End B/C's guard (`isCanonicalPathComponent`) are
  two separately-maintained function bodies with the identical contract, a change to either's
  rejection rule that isn't mirrored in the other silently reopens exactly this seam for whichever
  side falls behind.
- **Past break**: codex review on PR #798 (Phase 5b PR2), before merge — **Blocker**:
  `CreateTaskRequest.ID` is caller-supplied and saved as the literal DB primary key without
  validation (`internal/api/task_create.go`). A task literally IDed `"alias/../<victim-id>"` passed
  End D's string-equality check trivially (both sides carry the identical literal alias), while a
  bare `filepath.Join` (both End B/C's `AttachmentsRootForTask` *and*, independently, End A's bind
  construction) silently collapsed it down to the *victim's* real attachments directory — a
  cross-task leak reachable both via `boid task attachments list`/`get` (End B/C) and via the RO bind
  mounted into the attacker's own sandbox at dispatch time (End A). **Fixed in the same PR** for all
  three: `isCanonicalPathComponent` added to `AttachmentsRootForTask` (closes End B/C uniformly,
  since both route through it), and the duplicated `isCanonicalTaskIDComponent` added to gate End A's
  mount (a second, distinct commit on the same PR, after the bind-side half of this Blocker was
  initially scoped out and then flagged for a return decision — see the "When you touch it" section's
  history note). Also from the same review — **Major (TOCTOU)**: the original `ReadAttachment`
  validated symlink containment and the size cap via `filepath.EvalSymlinks`/`os.Stat` and then
  reopened the same path with `os.ReadFile`, leaving a swap window; fixed with a dirfd-relative
  `openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)` open-once-reuse-the-fd pattern on Linux
  (`attachment_read_linux.go`), falling back to a still-improved (single-`Open`, fd-reused)
  best-effort path on pre-5.6 kernels or non-Linux builds. **Minor**: `validateAttachmentLookupName`
  originally rejected any name merely *containing* `".."` as a substring, which was stricter than
  necessary (a separator-free basename can never traverse regardless of embedded dots) and created a
  write/read contract mismatch against `SanitizeAttachmentName`'s more permissive upload-time
  allowlist; loosened to share `isCanonicalPathComponent`'s "must not equal `.`/`..`, must not contain
  a separator" rule instead. **Nit**: `ListAttachments` originally admitted a symlink whose target
  stayed inside the directory, which the TOCTOU fix's categorical "no symlinks, ever" policy in
  `ReadAttachment` made inconsistent (list would show a name get could never return); `ListAttachments`
  now requires `info.Mode().IsRegular()` too, matching `ReadAttachment` exactly.
- **Guard**: End B/End C parity is enforced structurally (both call `AttachmentsRootForTask` with the
  same `dataHome`/`taskID`) — see `internal/api/attachments_test.go`'s `TestListAttachments_*`/
  `TestReadAttachment_*` for the filesystem-level behavior (path traversal, symlink escape — both the
  escaping and in-dir-but-still-rejected cases — the alias-`TaskID` cross-task-leak scenario, and the
  size cap) and `internal/server/boid_executor_task_attachments_test.go` for the executor-level wiring
  (a real temp `attachmentsRoot`, not a stub — there is no interface to bridge here, unlike seam #13's
  `jobContextProvider`). End A's rejection behavior is covered by
  `internal/dispatcher/sandbox_builder_test.go`'s
  `TestBuildSandboxSpec_AttachmentsBind_RejectsTraversalTaskID` (the alias/traversal-`TaskID` cases,
  asserting no mount at all is produced and specifically that no mount ever resolves to the victim
  directory). Separately, `internal/dispatcher/attachments_bind_parity_test.go`'s
  `TestAttachmentsBindSource_MatchesAPIHelper` (an external `dispatcher_test`-package test, since an
  internal one importing `internal/api` would itself be the cycle End A's doc note above describes)
  pins that End A's bind construction and `api.AttachmentsRootForTask` compute the *same path* for
  ordinary, already-canonical inputs — it does not (and cannot, without the cycle) directly assert
  End A and End B/C's two separate guard functions stay in lock-step; that is a manual-review
  obligation (see When you touch it). Broker-level authorization is covered by
  `internal/sandbox/broker_task_attachments_test.go`. The op ↔ escape-guard manifest
  (`internal/sandbox/broker_op_escape_test.go`) and the policy drift tests
  (`internal/orchestrator/policy_test.go`'s `wantOps`, `internal/dispatcher/policy_translate_test.go`'s
  `TestOpConstantsMirror`) all include the two new ops.
- **When you touch it**: if you touch `AttachmentsRootForTask`, `isCanonicalPathComponent`,
  `isCanonicalTaskIDComponent`, `dataHomeFor`, the `sandbox_builder.go` attachments mount, or
  `newBoidBuiltinExecutor`'s wiring in `wire.go`, verify all three path-construction call sites (End
  A/B/C) still resolve to the same directory for a given `(dataHome, taskID)` pair, that End A and
  End B/C's guard functions still reject the identical set of inputs, and re-run
  `TestAttachmentsBindSource_MatchesAPIHelper` plus
  `TestBuildSandboxSpec_AttachmentsBind_RejectsTraversalTaskID`. History note: the PR #798 codex
  review initially scoped the bind-side (End A) fix out as a Minor "doc accuracy" finding, on the
  reasoning that the original PR brief said not to touch `sandbox_builder.go` in this PR (that
  instruction was about avoiding *semantic* changes ahead of the 5b-6 cutover, not about exempting
  security fixes — flagged, and the scope was explicitly widened to include it before merge). At the
  5b-6 cutover (which retires End A's bind entirely, per the PR-6 note in the "PR 分割案 > 5b" section
  of docs/plans/phase5-shim-and-task-context.md), this seam collapses to just End B/C/D, and this
  entry should be reduced accordingly.

## 16. adapter-issued task-context RPC

Whether an *adapter's own Go code* (not the agent subprocess it forks) can actually reach the
Phase 5b task-context RPCs (seam #13) when it shells out to `boid` directly. Phase 5b PR3
(docs/plans/phase5-shim-and-task-context.md) added the first such caller: the claude adapter's
`readSessionsFromRPC` execs `boid task payload --field artifact.claude_code.sessions` from
inside `runner-inner-child` *before* the claude subprocess exists, so it cannot rely on
anything the agent's own `cmd.Env` overlay would otherwise guarantee — it has to build an
equivalent env (and rely on an equivalent PATH) itself, from the same `RunContext.Env` map.

- **End A (populate)**: `internal/dispatcher/sandbox_builder.go` sets `env["PATH"]` (via
  `buildPATH`, must include the shim-bin dir), `env["BOID_BUILTIN_SHIM"] = "1"`,
  `env["BOID_BROKER_SOCKET"]` / `env["BOID_BROKER_TOKEN"]`, and (via `setIfNonEmpty`)
  `env["BOID_TASK_ID"]` / `env["BOID_JOB_ID"]`. This whole map becomes `spec.Env`, which
  `internal/sandbox/runner/runner_linux.go`'s `runAgent` copies verbatim into
  `adapters.RunContext.Env` — and, separately, `RunInnerChild` does `os.Setenv("PATH",
  spec.Env["PATH"])` on the runner-inner-child process itself (the process the adapter's Go
  code — not just the forked agent — runs inside).
- **End B (consume)**: `claude.buildTaskPayloadSessionsCmd` (`internal/adapters/claude/run.go`)
  calls `exec.CommandContext(ctx, "boid", "task", "payload", "--field",
  "artifact.claude_code.sessions")` — `os/exec` resolves the bare name `"boid"` via
  `LookPath` against the **current process's** `PATH` env var at call time (not `cmd.Env`),
  so this depends on End A's `os.Setenv("PATH", …)` having already run. `cmd.Env` is then
  built by overlaying `rc.Env` on top of `os.Environ()` — the same map End A populated —
  which supplies `BOID_BUILTIN_SHIM` (routes the exec'd `boid` into `RunBoidShim` instead of
  the cobra CLI tree, see `main.go`'s `shouldRunBoidBuiltinShim`) and the four `BOID_TASK_ID` /
  `BOID_JOB_ID` / `BOID_BROKER_SOCKET` / `BOID_BROKER_TOKEN` vars the shim itself reads via
  `os.Getenv` (`runTaskContextShim`, seam #13's End B).
- **Invariant**: every env var `RunBoidShim`'s task-context path reads via `os.Getenv` must
  already be a key in `RunContext.Env` by the time an adapter's own Go code (not the forked
  agent) execs `boid`. `readSessionsFromRPC` does **not** swallow a broken link as "no
  sessions" — codex review on PR #800 (Major) caught the first version doing exactly that
  (mirroring the old file-based "missing payload.json → fresh start" contract, which was safe
  only because that read was 100% local and had no comparable failure mode): a transient
  broker hiccup would make `updateSessions` synthesize a fresh single-entry session list, and
  `writePayloadPatch` would then persist that truncated list over the task's real history,
  silently discarding every prior jsonl session id (see memory
  `phase3b-session-jsonl-not-persisted` for the earlier incident this rhymes with). The fixed
  contract: PATH missing the shim dir, `BOID_BUILTIN_SHIM` unset, or any of the four
  ids/socket/token vars dropped from `spec.Env` all surface as a non-nil error from
  `readSessionsFromRPC`, which `Run()` propagates immediately — aborting before claude ever
  starts and before `writePayloadPatch` touches disk. Only a genuinely empty `--field` result
  (exit 0, empty stdout — the field really doesn't exist yet) is `(nil, nil)`.
- **Guard**: End A is exercised transitively by every existing `sandbox_builder_test.go` test
  that asserts `env["BOID_BROKER_SOCKET"]` etc. End B (pure, no process spawn) =
  `TestBuildTaskPayloadSessionsCmd_Args` / `_EnvOverlaysRunContextEnv`
  (`internal/adapters/claude/run_test.go`); the error-propagation contract itself is
  `TestReadSessionsFromRPC_FetchErrorPropagates` / `_MalformedJSONPropagatesError` /
  `_EmptyFieldReturnsNilNoError` plus the `Run()`-level
  `TestRun_SessionsFetchError_AbortsBeforeStartingClaude` (asserts `payload_patch.json` is
  never written). The full chain (`os/exec` PATH resolution + `BOID_BUILTIN_SHIM` routing + a
  real fake-broker unix socket enforcing the token, not an injected fetch func) is
  `TestReadSessionsFromRPC_EndToEnd` plus its two negative siblings
  `_MissingBuiltinShimFails` / `_WrongTokenFails` (`internal/adapters/claude/run_rpc_wiring_test.go`),
  which re-exec the compiled test binary itself as the "boid" program on `PATH` (the
  `os/exec_test.go` `TestHelperProcess` idiom) so they never need a separately built binary.
  The first cut of this file's `TestMain` helper called `RunBoidShim` unconditionally and the
  fake broker never checked `req.Token` — codex review's Minor 1 on PR #800 caught both,
  which is exactly the failure mode `_MissingBuiltinShimFails` / `_WrongTokenFails` now pin.
- **When you touch it**: if you touch `sandbox_builder.go`'s env population (particularly
  `BOID_BUILTIN_SHIM` / `BOID_BROKER_SOCKET` / `BOID_BROKER_TOKEN` / `BOID_TASK_ID` /
  `BOID_JOB_ID` / the shim-bin entry in `buildPATH`), or add a second adapter-issued `boid
  task ...` call (e.g. a future codex/opencode Go-level RPC call, not just their bootstrap
  prompt text), re-run `TestReadSessionsFromRPC_EndToEnd`-shaped coverage rather than trusting
  the adapter-unit layer alone — the unit tests stub `fetchTaskPayloadSessions`/inject env
  directly and cannot catch a PATH or env-population regression upstream in the dispatcher. If
  you add a new adapter-issued RPC call, give it the same fetch-error-vs-empty-result
  distinction `readSessionsFromRPC` has — collapsing "the call failed" into "there was nothing
  there" is the specific bug class this seam exists to prevent. 5a-3 (fixed shim directory)
  changes *where* the shim-bin dir lives on PATH but not this seam's shape; 5b-6
  (file-distribution cutover) does not touch this seam either, since it never depended on the
  file side.

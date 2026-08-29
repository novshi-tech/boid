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
13. [task-context RPC (build ↔ serve)](#13-task-context-rpc-build--serve)
14. [shim command-name resolution (sandboxShimBinDir symlinks ↔ broker Commands key)](#14-shim-command-name-resolution)
15. [attachments RPC write ↔ read path](#15-attachments-rpc-write--read-path)
16. [adapter-issued task-context RPC (claude readSessionsFromRPC ↔ sandbox env)](#16-adapter-issued-task-context-rpc)
17. [payload_patch direct-pass merge parity, persistence, and concurrency](#17-payload_patch-direct-pass-merge-parity-persistence-and-concurrency)
18. [engine wait response → RuntimeExit → job status/diagnostics](#18-engine-wait-response--runtimeexit--job-statusdiagnostics)
19. [config schema seam (schema leaf → Config field → extractor → buildStartConfig → consumer)](#19-config-schema-seam)
20. [WorkspaceMeta write-path field coverage (strict PUT vs envelope apply)](#20-workspacemeta-write-path-field-coverage)
21. [apigateway SecretResolver namespace threading (sibling of #11)](#21-apigateway-secretresolver-namespace-threading)
22. [orchestrator.Action → timeline.Build renderability](#22-orchestratoraction--timelinebuild-renderability)
23. [oauth_providers config↔apigateway wiring (type mirror + CredentialProvider.oauth)](#23-oauth_providers-configapigateway-wiring)
24. [OAuthLoginHandler ↔ apiGatewayLoginAdapter ↔ apigateway.LoginManager](#24-oauthloginhandler--apigatewayloginadapter--apigatewayloginmanager)
25. [orchestrator.Action.Actor ctx propagation](#25-orchestratoractionactor-ctx-propagation)
26. [brokered op scoping layer (broker request-shaping ↔ executor re-check)](#26-brokered-op-scoping-layer-broker-request-shaping--executor-re-check)
27. [park rule set ↔ Wake origin resolution](#27-park-rule-set--wake-origin-resolution)
28. [status tab literal ↔ ListTasks status predicate — REMOVED (PR-4)](#28-status-tab-literal--listtasks-status-predicate--removed-pr-4-2026-08-26)
29. [Trigger.On due predicate ↔ stuck-detection (trackSkipStreak) invariant](#29-triggeron-due-predicate--stuck-detection-trackskipstreak-invariant)

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

### 5b. egress proxy port stability

Whether each workspace's proxy listener comes back on the **same port** after a daemon restart
(`docs/plans/egress-proxy-stable-port.md`). A drifting port silently breaks any job-side config
that baked the proxy URL — `~/.npmrc` is the one that has actually bitten — and the job-side
symptom is a slow retry loop, not an error.

- **End A**: `sandbox.egress_proxy_port_low`/`_high` in config.yaml →
  `config.SandboxConfig` → `cmd/start.go`'s `buildStartConfig` → `server.Config`.
- **End B**: `New()` in `internal/server/server.go` assigning
  `proxyManager.PortStore` / `PortRangeLow` / `PortRangeHigh`, consumed by
  `startStable`/`walkBand` in `internal/sandbox/proxy_manager.go` and persisted through
  `orchestrator.EgressPortStore` (`workspace_egress_port`).
- **Invariant**: a key's port is stable across restarts; the allocator never takes a port
  another key has **reserved** while any unreserved port remains (listeners are created lazily
  at dispatch, so a reserved port with nothing listening is normal, not free); every port change
  is logged with both old and new numbers; exhaustion degrades to ephemeral rather than failing
  dispatch.
- **When you touch it**: the classic drift here is testing both ENDS and not the JOIN — delete
  the three assignments in `New()` and the whole feature silently reverts to ephemeral ports
  with every test still green. Check `TestNew_WiresEgressProxyPortStore` still covers it. Also
  check that anything new which deletes a workspace releases its `workspace_egress_port` row
  (no FK cascade exists — the key space includes the non-slug `__no_workspace__`).

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
  agent-facing surface (`BuildWorkspaceEnvView` in `internal/dispatcher/workspace_env_view.go`,
  served by the `boid task env` RPC — the dispatch-time `environment.yaml` file this used to
  also feed was retired by the Phase 5b PR6 cutover, see seam #13) intentionally shows a
  **subset** (no path/env) — don't "fix" that asymmetry, but do keep reject rules visible to
  the agent.

- **5a-3 landed note (2026-07-21, PR #TBD)**: `SandboxRuntimeInfo.ResolvedHostCommands`
  (the byPath sibling of `ResolvedHostCommandsByName`) was deleted in the cutover: no
  downstream consumer keys off the absolute host path any more — `hostCommandMounts` and
  `buildHostCommandNamesEnv` went with it, and `buildPATH` collapsed to a single
  `sandboxShimBinDir` entry. `ResolveHostCommands` still returns the byPath map as an
  inert byproduct (dedup filter still uses it internally) but no production caller reads
  it. See seam #14 for the shim/broker side of the same cutover.

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

## 13. task-context RPC (build ↔ serve)

**Retired half (history)**: through Phase 5b PR5, this seam also covered a second, parallel
path — dispatch-time context files (`$HOME/.boid/context/{task,instructions,environment,
payload}.{yaml,json}`, written by `contextFiles`/`buildEnvironmentYAML` in
`internal/dispatcher/sandbox_builder.go`) that had to keep serving the *same* data as the RPCs
below. Phase 5b PR6 (the "file 配布経路そのものを撤去する目玉 cutover",
docs/plans/phase5-shim-and-task-context.md) deleted `contextFiles`/`buildEnvironmentYAML`/
`marshalTaskYAML`/`marshalInstructionsYAML`/`EnvironmentInput` outright — there is no more file
side to drift out of sync with, and the two-Ends-plus-file-materialization shape this entry used
to have (End A = file, End B = RPC build, End C = serve) collapsed to the two Ends below. If
you're reading old context (a PR description, a code comment) that says "End A (file)" or
"End C (serve)" for this seam, that's the pre-PR6 shape — this entry now uses End A = build, End
B = serve.

Whether `Runner.trackJobContext`'s snapshot and what `boid task instructions`/`env`/`payload`
actually return stay internally consistent — i.e. whether the RPC's own build and serve sides
agree, now that there is no separate file to check them against. The scoping key differs by RPC
and getting it wrong is the actual failure mode this seam has already produced (see **Past
break**): `boid task current` is TaskID-scoped (safe — re-derives live from the task row, no
per-job ambiguity); `boid task instructions` / `env` / `payload` are all **JobID-scoped**, because
their source data is job-scoped, not task-scoped.

- **End A (build)**: `Runner.trackJobContext` (`internal/dispatcher/job_context.go`), called in
  `Runner.Dispatch` right after `resolveWorkspaceProxy`, builds a
  `JobContextSnapshot{Instructions: routedInstructionSlice(spec.Instruction), Env:
  BuildWorkspaceEnvView(allowedDomains, spec.HostCommands), Payload: spec.PrimaryInput}` — every
  field sourced straight from *this exact job's* JobSpec values, never re-derived from the task
  row. `Instructions` is populated **iff** `JobSpec.Instruction != nil` (this job's own routed
  instruction — `orchestrator.DispatchPlanner.PlanHook`'s `selectInstruction`, filtered by *this
  hook's* declared agent); `Env.HostCommands` is fed by `spec.HostCommands` (the
  short-name-keyed map — as of Phase 5 5a-3 the byName view is the sole
  resolved-host-command shape any code path keys off; the pre-5a-3 byPath sibling
  `SandboxRuntimeInfo.ResolvedHostCommands` is retired, see seam #14's landed note) via the shared
  `convertHostCommands` helper;
  `Env.AllowedDomains` comes from the `allowedDomains` local in `Runner.Dispatch`; `Payload` is
  `JobSpec.PrimaryInput` (already trait-filtered by `orchestrator.FilterPayloadByTraits` at plan
  time, per the firing hook's declared `Traits.Consumes`). `boid task current` instead re-derives
  live from the task row (`orchestrator.SnapshotTask`) since that data has no job-scoped filtering
  dependency — see `internal/api/task_context.go`'s package doc comment for why only `current`
  gets that treatment.
- **End B (serve)**: `boidBuiltinExecutor.ExecuteBoidBuiltin`'s `BoidOpTaskInstructions` /
  `BoidOpTaskEnv` / `BoidOpTaskPayload` cases (`internal/server/boid_executor.go`) read back via
  the `jobContextProvider` interface, which `*dispatcher.Runner` satisfies structurally — wired in
  `internal/server/wire.go`'s `newBoidBuiltinExecutor(..., runner)` call using the **same**
  `runner` variable `Dispatch` runs on, not a separate instance. `internal/sandbox/broker.go`
  authorizes these three ops by strict `JobID` equality against the token's own context (never
  `TaskID`) — `BoidOpTaskCurrent` alone is authorized by `TaskID`.
- **Invariant**: End A must derive from the job's own JobSpec values (`spec.Instruction`,
  `spec.HostCommands`, `spec.PrimaryInput`, `allowedDomains`), never the task row — a future
  refactor that re-derives any of them from the task row breaks correctness even though the code
  still "works" and compiles, because a task can have **multiple concurrent/sequential jobs whose
  routed instructions differ** (see Past break). `JobContextSnapshot` must not outlive its job:
  `Runner.UnregisterJob` must clear it (mirrors the broker token's own lifecycle).
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
     jobs gets a non-nil `Instruction`. The task-row derivation had no way to tell the two jobs
     apart and would hand the wrong job the other agent's instruction. Fixed by moving
     `Instructions` into `JobContextSnapshot` (JobID-scoped, same pattern as `Env`/`Payload`) before
     merge — see `orchestrator.CurrentInstructions`'s doc comment for what it's safe (and unsafe)
     to use for now.
- **Guard**: End A = `TestDispatch_TracksJobContext_Instructions_MatchesJobSpec` /
  `_NilJobSpecInstructionYieldsEmpty`, `_EnvAndPayload`, `_NilPrimaryInput`,
  `TestUnregisterJob_RemovesJobContext` (`internal/dispatcher/runner_job_context_test.go`), plus
  `TestPlanHook_Instruction_MatchingAgent` / `_NonMatchingAgent_ReturnsNil`
  (`internal/orchestrator/planner_test.go` — the latter is the root-cause case: a hook whose agent
  doesn't match the active history entry gets `Instruction == nil` even though the evaluator fired
  it). End B = the `TestBoidBuiltinExecutor_Task{Instructions,Env,Payload,Current}_*` suite
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
- **When you touch it**: if you touch `selectInstruction`/`Evaluator.Evaluate`,
  `Runner.Dispatch`'s `trackJobContext` call, or `jobContextProvider`/`newBoidBuiltinExecutor`'s
  wiring in `wire.go`, verify End A still reads from the **per-job** JobSpec values (never the task
  row, for anything but `BoidOpTaskCurrent`) and that the real-Runner wiring tests still exercise
  the exact `runner` instance `wire.go` threads through. Any change that makes a task-context RPC
  read from the task row should immediately raise the question this seam exists to ask: "can two
  jobs from this same task disagree about this value?" — if yes, it must be JobID-scoped.
- **Update (Phase 5b PR4, history)**: `TaskSnapshot` gained a `Readonly` field
  (`internal/orchestrator/jobspec.go`) so `boid task current` — and, through it, the boid-task
  skill's Step 0 mode determination — can read `readonly` without falling back to the (then still
  file-based, now fully retired) `environment.yaml`.
- **Update (Phase 5b PR5, history)**: `buildEnvironmentYAML`'s `environment.yaml` was reduced to
  the exact same `WorkspaceEnvView` `BuildWorkspaceEnvView` builds for `BoidOpTaskEnv` — the two
  could no longer drift on *data*, only on wire-format (the file was a direct struct marshal; the
  RPC's CLI-side re-render round-trips through JSON first, so field order differed). Moot since
  PR6 deleted the file side outright.
- **Update (Phase 5b PR6)**: the file side (`contextFiles`/`buildEnvironmentYAML`/
  `marshalTaskYAML`/`marshalInstructionsYAML`/`EnvironmentInput`) is deleted. The `$HOME/.boid`
  job-scoped tmpfs overlay that isolated its writes is **not** deleted, despite an early cut of
  this PR trying to: codex review (Blocker + Major, before merge) found that with the overlay gone,
  the one remaining writer under `$HOME/.boid` — `$HOME/.boid/output/payload_patch.json`, the
  `job_done` file fallback (decision 6, primary at the time this PR landed — 5b PR7 later added
  the RPC direct-pass path as the new primary, but deliberately kept this file fallback alive; see
  seam #17) — became a fixed, shared path on the persistent workspace home, letting concurrent
  jobs in the same workspace delete/merge each other's patches, and letting a prior job's ancestor
  symlink redirect a later job's dispatch-time file operations outside the intended directory.
  Restoring the tmpfs (rather than hardening the operations that ran on top of it) closes both
  classes of attack structurally — see `docs/plans/phase5-shim-and-task-context.md`「PR 分割案 >
  5b」6's landed note and `internal/dispatcher/sandbox_builder.go`'s `homeMounts` doc comment for
  the full history. The overlay must survive until the file-based `job_done` fallback is retired
  outright — 5b PR7 did NOT do this (its own scope keeps the fallback alive); that retirement is
  deferred to a later phase (Phase 6 backend-swap era). `SandboxRuntimeInfo.WorkspacePeerAdvertise`
  and `Runner.buildPeerAdvertise` (`gitgateway_wire.go`) — the data the file's now-gone
  `workspace_projects` section used to carry — were kept as-is, still computed, still unconsumed by
  `BuildSandboxSpec`, for several PRs: a deliberate call to continue the "carried but inert across a
  PR boundary" pattern rather than invent a new consumer, pending a future `boid workspace
  peers`-style RPC (tracked as an open item in the plan doc). **2026-08 update**: that RPC landed,
  but not as an independent `boid workspace peers` command — `boid project list` was extended
  instead (`clone_url`/`reference_path`/`clone_dir` on peer entries), reading
  `Runner.buildPeerAdvertise`'s output through a *different* carrier
  (`JobContextSnapshot.WorkspacePeerAdvertise`, `internal/dispatcher/job_context.go`, tracked by
  `Runner.Dispatch`). `SandboxRuntimeInfo.WorkspacePeerAdvertise` itself stays exactly as inert as
  before — it was never the thing wired up — so this is a genuine dead-code cleanup candidate now,
  not a "future consumer pending" placeholder. `e2e/scenarios/git-gateway-peer-fetch` no longer
  exists at all: PR-4's userns/dir-based black-box harness removal
  (`docs/plans/volume-only-daemon.md` §論点e) deleted the whole `e2e/scenarios/` tree it lived in,
  so the "don't remove the skip without a replacement" caution above is moot — there is no scenario
  left to un-skip.

## 14. shim command-name resolution

Whether a shim invocation inside the sandbox identifies itself to the broker under the same
key the broker's `Commands` map is registered with. As of the **5a-3 cutover**
(`docs/plans/phase5-shim-and-task-context.md`, "5a: shim 固定ディレクトリ化" PR3) this is
purely a structural property: every shim is a symlink at
`<dispatcher.sandboxShimBinDir>/<declared name>` pointing at the boid multi-call binary
(also bind-mounted once at `<sandboxShimBinDir>/boid`), so the shim's argv0 basename ==
declared short name == broker Commands map key by construction. The pre-5a-3
BOID_HOST_COMMAND_NAMES env-map + `ResolveShimCommandName` bridge that used to bridge the
aliased-file-basename case (e.g. `host_commands.run-e2e.path: e2e/run.sh`) and the broker's
Path-scan fallback were both retired in the same change — the alias case now resolves at
the dispatcher (symlink named after the declared name), not inside the shim.

- **End A (dispatcher, materialize)**: `hostCommandSymlinks` in
  `internal/dispatcher/sandbox_builder.go`, fed by `rt.ResolvedHostCommandsByName` (the
  **byName** view of `dispatcher.ResolveHostCommands` — the same short-name-keyed map fed to
  the broker at End C). Emits one `sandbox.Symlink{LinkPath:
  sandboxShimBinDir+"/<name>", LinkTarget: "boid"}` per entry. The boid binary is
  separately bind-mounted at `sandboxShimBinDir+"/boid"`; the runner-inner-child creates
  the symlinks after pivot_root under that same directory. ProfileInit is exempt — the
  host `/` rbind already exposes boid and no host commands are declared.
- **End B (shim, resolve)**: `sandbox.CommandFromArgv0(os.Args[0])` (`internal/sandbox/shim.go`),
  called once per invocation from `main.go`'s `shimMain`. Just `filepath.Base(argv0)` — no
  env-map lookup, no side channel; the bind-mount basename is authoritative. The **same**
  resolved name feeds both `EarlyRejectFromEnv` (the shim-side fast-path reject check) and
  `ShimExec`'s `ExecRequest.Command`.
- **End C (broker, authorize)**: `entry.Commands` in `internal/sandbox/broker.go`, registered
  under the byName view (`dispatcher.CommandBroker.RegisterCommands`'s short-name-keyed
  input — see seam #7's sibling wiring). `lookupCommand` is a direct short-name key lookup;
  the pre-5a-3 Path-scan fallback (kept intentionally through 5a-2 as a rollback safety net)
  was dropped alongside the 5a-3 cutover.
- **Invariant**: for every `host_commands` entry, `hostCommandSymlinks`'s LinkPath basename
  must equal the same `def.Name` the broker's Commands map is keyed by at End C — both derive
  from the single `ResolveHostCommands` call's byName map, so a future refactor that lets End
  A and End C diverge onto two different resolved-command maps would silently reject every
  host command whose symlink name and broker key desynchronised. Non-aliased entries
  (basename already equals declared name) hide such a break; the alias-echo case in the e2e
  guard below is the direct-observation regression net.
- **Guard**: End A = `TestBuildSandboxSpec_HostCommandSymlinks_UnderShimBinDir`,
  `TestHostCommandSymlinks_AliasedPathUsesDeclaredName`,
  `TestHostCommandSymlinks_LinkPathIsShimBinDirSlashName`, and
  `TestBuildSandboxSpec_ShimBinDirBoidMount(SkippedForProfileInit)`
  (`internal/dispatcher/sandbox_builder_test.go`). End B = `TestCommandFromArgv0`
  (`internal/sandbox/shim_test.go`). End C = `TestBroker_ShortNameKeyedCommand_DirectMatch`,
  `TestBroker_ShortNameKeyedCommand_AliasDirectMatch`,
  `TestBroker_ShortNameKeyedCommand_AbsolutePathRejected`
  (`internal/sandbox/broker_test.go`; the last one is the affirmative
  cutover-negative-case — pre-5a-3 this returned success via Path scan) and
  `TestBroker_StreamingAbsolutePathRejected` (`broker_streaming_test.go`). Full end-to-end
  (real sandbox, real shim binary, aliased `host_commands` entry) remains covered by
  `e2e/scenarios/host-command-smoke`'s `alias-echo` command
  (`e2e/fixtures/kits/host-ops/kit.yaml`, invoked in the sandbox as its declared name
  `alias-echo` — the file's actual basename is `echo-target`, never used).
- **When you touch it**: if you touch `hostCommandSymlinks`, `sandboxShimBinDir`,
  `CommandFromArgv0`, `ResolveHostCommands`'s byName view, `lookupCommand`, or the
  runner-inner-child symlink materialization loop, verify a request through an **aliased**
  `host_commands.<name>.path` entry still resolves — the non-aliased case (basename ==
  declared name) passes even when the alias-specific wiring is broken, so it is not a
  sufficient test on its own. The 5a-3 landed shape is *symlink name = declared name*, no
  env side channel — if you find yourself re-introducing BOID_HOST_COMMAND_NAMES or a
  broker Path-scan fallback, that's a signal the invariant above has been broken elsewhere;
  fix the underlying divergence rather than restoring the bridge.

- **5a-3 landed note (2026-07-21, PR #806)**: this seam collapsed to the shape above in the
  cutover: BOID_HOST_COMMAND_NAMES + ResolveShimCommandName + shimBinaryPath +
  buildHostCommandNamesEnv + `SandboxRuntimeInfo.ResolvedHostCommands` (byPath field) +
  the broker `lookupCommand` Path-scan fallback all landed as deletions in one PR. The
  aliased-basename attack class (Ends A/B divergence at the argv0 boundary) is now
  structurally impossible: the shim's bind-mount name IS the declared name.

## 15. attachments RPC write ↔ read path

**Retired half (history)**: through Phase 5b PR5, this seam also covered a third path — a
dispatch-time RO bind (`~/.boid/attachments`, gated by `isCanonicalTaskIDComponent` in the
now-deleted `internal/dispatcher/attachments_path.go`) that had to resolve to the identical
directory the RPC read/write paths below use. Phase 5b PR6 (the "file 配布経路そのものを撤去する
目玉 cutover", docs/plans/phase5-shim-and-task-context.md) deleted that bind and
`attachments_path.go` outright, per the PR-6 note this entry used to carry — `boid task
attachments list`/`get` are now the sole in-sandbox read path. The old three-Ends-plus-bind shape
(End A = bind, End B = write path, End C = RPC read path, End D = authorization) collapsed to the
two Ends below (kept as B/C/D's original letters, minus the retired A, to avoid relettering
churn against old PR history that cites them).

Whether the Phase 5b PR2 attachments RPCs (`boid task attachments list` / `get <name>`,
docs/plans/phase5-shim-and-task-context.md) read from the identical on-disk directory the upload
path writes to.

- **End B (write path)**: `EnsureAttachmentsDir`/`SaveMultipartAttachments`
  (`internal/api/attachments.go`, called from `web.go`'s upload handlers) resolve the directory via
  `AttachmentsRootForTask(dataHome, taskID)`, which rejects a non-canonical `taskID` via
  `api.isCanonicalPathComponent`.
- **End C (RPC read path)**: `api.ListAttachments` / `api.ReadAttachment`
  (`internal/api/attachments.go`), called from `boidBuiltinExecutor`'s
  `BoidOpTaskAttachmentsList`/`Get` cases (`internal/server/boid_executor.go`), resolve through the
  same `AttachmentsRootForTask` as End B. The executor's `attachmentsRoot` field is threaded in
  `wire.go`'s `newBoidBuiltinExecutor(..., dataHomeFor(cfg))` call — the identical `dataHomeFor(cfg)`
  expression End B's callers use.
- **End D (authorization)**: `internal/sandbox/broker.go`'s `BoidOpTaskAttachmentsList`/`Get` case
  authorizes by strict `TaskID` *string equality* against the token's own context (same pattern as
  `BoidOpTaskCurrent`) — it never resolves a filesystem path, so it cannot itself catch a
  traversal-shaped `TaskID`; that is End B/C's job.
- **Invariant**: End B and End C must resolve to the identical directory for a given
  `(dataHome, taskID)` pair — trivially true today since both route through the same
  `AttachmentsRootForTask` helper, so this can only break by one side stopping to call it — AND
  reject the same set of non-canonical `taskID` values (empty, containing a path separator, or the
  literal `.`/`..`) before ever constructing a path — a `taskID` that passes End D's raw
  string-equality check must never be allowed to resolve, via `filepath.Join`'s automatic
  `..`-collapsing, to a *different* task's directory (see Past break for the concrete exploit this
  produces when the guard is missing).
- **Past break**: codex review on PR #798 (Phase 5b PR2), before merge — **Blocker**:
  `CreateTaskRequest.ID` is caller-supplied and saved as the literal DB primary key without
  validation (`internal/api/task_create.go`). A task literally IDed `"alias/../<victim-id>"` passed
  End D's string-equality check trivially (both sides carry the identical literal alias), while a
  bare `filepath.Join` (`AttachmentsRootForTask`, shared by End B/C, plus — at the time —
  independently in the now-deleted dispatch-time bind) silently collapsed it down to the *victim's*
  real attachments directory. **Fixed in the same PR**: `isCanonicalPathComponent` added to
  `AttachmentsRootForTask` closes End B/C uniformly, since both route through it. (The bind-side
  half of this same Blocker, fixed in the same PR via the now-deleted `isCanonicalTaskIDComponent`,
  is moot since PR6 deleted the bind outright.) Also from the same review — **Major (TOCTOU)**: the
  original `ReadAttachment` validated symlink containment and the size cap via
  `filepath.EvalSymlinks`/`os.Stat` and then reopened the same path with `os.ReadFile`, leaving a
  swap window; fixed with a dirfd-relative `openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`
  open-once-reuse-the-fd pattern on Linux (`attachment_read_linux.go`), falling back to a
  still-improved (single-`Open`, fd-reused) best-effort path on pre-5.6 kernels or non-Linux builds.
  **Minor**: `validateAttachmentLookupName` originally rejected any name merely *containing* `".."`
  as a substring, which was stricter than necessary (a separator-free basename can never traverse
  regardless of embedded dots) and created a write/read contract mismatch against
  `SanitizeAttachmentName`'s more permissive upload-time allowlist; loosened to share
  `isCanonicalPathComponent`'s "must not equal `.`/`..`, must not contain a separator" rule instead.
  **Nit**: `ListAttachments` originally admitted a symlink whose target stayed inside the directory,
  which the TOCTOU fix's categorical "no symlinks, ever" policy in `ReadAttachment` made
  inconsistent (list would show a name get could never return); `ListAttachments` now requires
  `info.Mode().IsRegular()` too, matching `ReadAttachment` exactly.
- **Guard**: End B/End C parity is enforced structurally (both call `AttachmentsRootForTask` with the
  same `dataHome`/`taskID`) — see `internal/api/attachments_test.go`'s `TestListAttachments_*`/
  `TestReadAttachment_*` for the filesystem-level behavior (path traversal, symlink escape — both the
  escaping and in-dir-but-still-rejected cases — the alias-`TaskID` cross-task-leak scenario, and the
  size cap) and `internal/server/boid_executor_task_attachments_test.go` for the executor-level wiring
  (a real temp `attachmentsRoot`, not a stub). Broker-level authorization is covered by
  `internal/sandbox/broker_task_attachments_test.go`. The op ↔ escape-guard manifest
  (`internal/sandbox/broker_op_escape_test.go`) and the policy drift tests
  (`internal/orchestrator/policy_test.go`'s `wantOps`, `internal/dispatcher/policy_translate_test.go`'s
  `TestOpConstantsMirror`) all include the two new ops.
- **When you touch it**: if you touch `AttachmentsRootForTask`, `isCanonicalPathComponent`,
  `dataHomeFor`, or `newBoidBuiltinExecutor`'s wiring in `wire.go`, verify End B/C still resolve to
  the same directory for a given `(dataHome, taskID)` pair and still reject the identical set of
  inputs.

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

## 17. payload_patch direct-pass merge parity, persistence, and concurrency

Whether `boid task update --payload-patch @-` (Phase 5b PR7, docs/plans/phase5-shim-and-task-
context.md decision 6/7) reproduces the file-based `payload_patch.json` → `job_done` → hook-
completion pipeline's merge *semantics* (trait allowlist, merge mode) — AND whether the direct
write it makes mid-job actually *survives* the rest of that same job's own completion-time persist
step, and survives a second concurrent caller doing the same thing. The first cut of this RPC got
the semantics right but shipped with three real bugs codex review caught before merge: a TOCTOU
staleness bug in how the trait allowlist was derived, a non-transactional read-modify-write race
between two concurrent callers, and — the most severe — a completely unrelated subsystem
(`internal/orchestrator/coordinator.go`'s hook-completion pipeline) silently reverting the RPC's
own successful write once the calling job's hook finished. All three are closed below. This is a
distinct seam from #13: #13 is about the four *read* RPCs staying internally consistent between
their own build and serve sides; this one is about a *write* path whose value must survive contact
with a completion-time persist step it does not control.

- **End A (dispatch-time trait capture — build)**: `orchestrator.DispatchPlanner.PlanHook`
  (`internal/orchestrator/planner.go`) sets `JobSpec.HookTraitsProduces = event.Hook.Traits.Produces`
  verbatim, at the moment the firing hook is resolved — the SAME value
  `HandlerResult.allowedTraits(matchedHooks)` (`coordinator.go`) would apply to that same hook's
  file-based payload_patch merge. `Runner.Dispatch`'s `trackJobContext` call
  (`internal/dispatcher/runner.go`) threads it into
  `JobContextSnapshot.PayloadPatchAllowedTraits` (`internal/dispatcher/job_context.go`), the exact
  per-job/JobID-scoped structure seam #13 already established for `task env`/`instructions`/
  `payload`. nil means unrestricted — true for both a virtual/synthesized agent-kind hook
  (`orchestrator.synthesizeAgentHook`, fired whenever a behavior declares no explicit hook of its
  own — the common case, e.g. boid's own `.boid/project.yaml` — whose `Traits` field is always the
  zero value) and an explicitly declared hook with no `traits.produces` list; both are
  indistinguishable from "unrestricted" on the file-based path too.
- **End B (merge — serve)**: `boidBuiltinExecutor`'s `BoidOpTaskUpdatePayloadPatch` case
  (`internal/server/boid_executor.go`) reads `snap.PayloadPatchAllowedTraits` from
  `e.jobContexts.JobContext(req.JobID)` — erroring if no context is tracked for that job, the same
  contract `TaskInstructions`/`Env`/`Payload` already have — and passes it straight through to
  `api.TaskAppService.UpdateTaskPayloadPatch(jobID, patch, allowedTraits)`
  (`internal/api/task_service.go`), which calls the SAME `orchestrator.MergePayloadPatch` function
  the file-based pipeline calls, serialized per task id (see Concurrency below) against the live
  task row. Reached via `boid task update --payload-patch @-|@<file>|<inline>`
  (`internal/sandbox/boid_shim.go`'s `parseBoidTaskUpdatePayloadPatch`).
- **Invariant (A↔B)**: End B's `allowedTraits` must be *exactly* the value End A captured for this
  job at dispatch time — never re-derived from a live project-meta/behavior/hook lookup at merge
  time. **Past break (Major 1, codex review before merge)**: an early cut of `UpdateTaskPayloadPatch`
  did exactly that live re-lookup (current project meta → `LookupBehaviorWithAlias` → search
  `behavior.Hooks` by `job.HandlerID`). Since project.yaml can be edited/reloaded between dispatch
  and the RPC call, that lookup could apply a since-edited (narrower or wider) trait list, or fail
  to find a renamed/removed hook and silently fall back to unrestricted even though the hook that
  actually fired this job had a real restriction. Accepting the dispatch-time value as a parameter
  (rather than re-deriving it) makes the whole staleness class structurally impossible instead of
  requiring a "fail closed on lookup failure" special case.
- **Concurrency (Blocker 2)**: `UpdateTaskPayloadPatch`'s `GetTask → MergePayloadPatch → UpdateTask`
  is a read-modify-write over the full task row. **Past break**: with no serialization, two
  concurrent calls for the SAME task (e.g. two hooks in the same readonly task's parallel dispatch
  round, each patching a different trait) can both read the same pre-write snapshot, and the second
  caller's full-row `UpdateTask` silently discards the first's write (and any other field — status,
  awaiting — that changed in between). Fixed with `payloadPatchLockFor(taskID)`
  (`internal/api/payload_patch_lock.go`): a fixed 64-shard mutex array keyed by a hash of the task
  id, wrapping the whole critical section. Deliberately NOT a `map[string]*sync.Mutex` (would grow
  forever over a long-running daemon's lifetime) and deliberately much narrower in duration than the
  retired per-task branch lock (memory: khi-supervisor-branch-lock-headline-block) — this lock's
  critical section is only the handful of DB calls inside one `UpdateTaskPayloadPatch` call, not a
  task's entire executing lifetime. Scope: this closes concurrent `UpdateTaskPayloadPatch` calls
  racing against EACH OTHER only — it does not serialize against every other task-row writer in the
  codebase (`ApplyAction`, `NotifyTask`, the existing `--payload-file` `UpdateTask`); closing that
  fully general problem needs optimistic-concurrency versioning on every task write path, out of
  scope here.
- **Persistence (Blocker 1, the most severe finding)**: a completely different subsystem —
  `internal/api/workflow_action.go`'s `runDispatchLoop` and `internal/api/workflow_replay.go`'s
  `ReplayHook` caller — persists the RESULT of the same dispatch cycle the RPC-writing job belongs
  to, once that job's hook completes. **Past break**: both callsites merged (`runDispatchLoop`) or
  wholesale-assigned (`ReplayHook`'s caller — even blunter, not even a merge) the coordinator's
  `DispatchResult.FinalPayload`/`ReplayResult.FinalPayload` onto a freshly re-read task row.
  `FinalPayload` is built from a snapshot of `task.Payload` taken BEFORE the hook ran, with only
  THIS cycle's hook `PayloadPatch`es merged on top — so for a reopened task (non-empty payload at
  round start) whose agent job wrote a NEW report via `--payload-patch` and then exited with no
  file-based output of its own, `FinalPayload` is just the STALE pre-reopen snapshot, unchanged.
  Applying it onto the freshly re-read row (which already has the RPC's successful write) silently
  reverted the report back to its pre-reopen value — a **CLI call that returned success getting
  invisibly undone** once the surrounding job finished. Fixed by adding
  `DispatchResult.PayloadDelta`/`ReplayResult.PayloadDelta` (`internal/orchestrator/types.go`,
  `coordinator.go`): the SAME hook-patch merge as `FinalPayload`, but folded starting from an empty
  object instead of the stale snapshot — i.e. only what this cycle's hooks actually wrote. Both
  persist callsites now merge/apply `PayloadDelta` onto the freshly re-read row instead of
  `FinalPayload`; an empty delta (the common case for an agent reporting exclusively via the RPC) is
  a safe no-op (`orchestrator.MergePayload`'s own empty-update short-circuit returns the fresh row's
  payload unchanged), so a stale snapshot never gets a chance to overwrite anything. This is the
  same *shape* of bug `StripAwaitingTrait` already defended against for the `awaiting` trait
  specifically (see `runDispatchLoop`'s own comment history) — the delta fix generalizes that
  defense to every trait rather than just `awaiting`, and `StripAwaitingTrait` is kept applied to
  `PayloadDelta` too as an additional defensive layer, not removed.
- **Guard**: End A capture — `TestPlanHook_CapturesHookTraitsProduces` /
  `_HookTraitsProduces_NilForHookWithNoProduces` (`internal/orchestrator/planner_test.go`),
  `TestDispatch_TracksJobContext_PayloadPatchAllowedTraits` /
  `_NilJobSpecFieldYieldsNil` (`internal/dispatcher/runner_job_context_test.go`). Merge parity —
  `internal/api/task_payload_patch_test.go`'s `TestUpdateTaskPayloadPatch_MergesWhenTraitAllowed` /
  `_DropsTraitNotInProduces` / `_NilAllowedTraits_UnrestrictedMerge` / `_SharedTraitMergesByHandlerID`
  (allowedTraits is now a caller-supplied parameter — these exercise the merge directly, no lookup
  fixture needed). Executor wiring (real `JobContextSnapshot` sourcing, not a stub merge function) —
  `internal/server/boid_executor_task_update_payload_patch_test.go`'s `_HappyPath` /
  `_DropsTraitNotInDispatchTimeAllowlist` / `_JobContextNotTracked`. Concurrency —
  `internal/api/task_payload_patch_test.go`'s
  `TestUpdateTaskPayloadPatch_ConcurrentCallsDoNotLoseUpdates` (injected per-call latency inside a
  race-detector-clean fake store widens the interleaving window enough to make the lost-update race
  deterministically observable without the lock — confirmed to fail intermittently without
  `payloadPatchLockFor` and pass 100% of the time with it). Persistence —
  `internal/orchestrator/coordinator_payload_delta_test.go`'s
  `TestCoordinator_DispatchAndAdvance_PayloadDelta_EmptyWhenHookProducesNoOutput` /
  `_OnlyContainsThisCyclesWrites` (Coordinator-level delta contract), plus the end-to-end regression
  at the persist layer: `internal/api/workflow_action_payload_delta_test.go`'s
  `TestTaskWorkflowServiceRunDispatchLoop_PreservesMidHookRPCWrite_ReopenScenario` /
  `_MergesNonEmptyPayloadDeltaOntoFreshRow`, and `internal/api/task_hook_replay_test.go`'s
  `TestTaskWorkflowService_ReplayHook_PreservesMidHookRPCWrite` — all three confirmed to fail without
  their respective fix (reverted locally and re-run) before landing. Broker authorization (JobID
  strict equality, mirroring `BoidOpTaskInstructions`/`Env`/`Payload`) is
  `internal/sandbox/broker_task_update_payload_patch_test.go`, which also covers the
  `PayloadPatchMaxBytes` size-cap re-check (Major 3, below). The op ↔ escape-guard manifest
  (`internal/sandbox/broker_op_escape_test.go`) and the policy drift tests
  (`internal/orchestrator/policy_test.go`'s `wantOps`,
  `internal/dispatcher/policy_translate_test.go`'s `TestOpConstantsMirror`) both include the new op.
- **Adjacent fixes bundled into the same review round** (not this seam's own invariant, but worth
  knowing if you land nearby): **Major 2** — the CLI's YAML→JSON conversion
  (`boid_shim.go`'s `parseBoidTaskUpdatePayloadPatch`) originally didn't apply the same non-string-
  key normalization `orchestrator.coordinator.go`'s `parseHandlerResult` already had (the historical
  `on:` → `true:` PyYAML round-trip incident), so identical payload_patch content could behave
  differently — or fail to marshal outright — depending on whether it traveled via the file fallback
  or this CLI. Fixed by extracting the shared, dependency-free `internal/yamlutil.NormalizeKeys`
  (both `orchestrator` and `sandbox` now call the SAME implementation — `sandbox` cannot import
  `orchestrator`, hence the new leaf package). **Major 3** — `--payload-patch` content crosses the
  broker RPC boundary into the daemon process (unlike a purely local file read), so an unbounded
  `io.ReadAll` was a real OOM vector; fixed with `PayloadPatchMaxBytes` (10 MB, matching
  `api.AttachmentMaxFileBytes`'s Phase 5b PR2 precedent) enforced at BOTH the shim
  (`readPayloadPatchSource`, so an oversized input never reaches the wire) and the broker
  (`internal/sandbox/broker.go`'s `BoidOpTaskUpdatePayloadPatch` case, defense in depth against a
  shim bypass or a future less-careful caller).
- **When you touch it**: if you touch `JobSpec.HookTraitsProduces`, `JobContextSnapshot.
  PayloadPatchAllowedTraits`, `HandlerResult.allowedTraits`, `MergePayloadPatch`,
  `orchestrator.synthesizeAgentHook`, or `TaskAppService.UpdateTaskPayloadPatch`, verify End A and
  End B still agree on what "the traits this job's hook may produce" means — a live re-lookup
  anywhere in this chain reintroduces the TOCTOU bug. If you touch `DispatchResult.PayloadDelta` /
  `ReplayResult.PayloadDelta`, `Coordinator.DispatchAndAdvance`/`ReplayHook`'s hook-loop, or either
  persist callsite (`runDispatchLoop`, `workflow_replay.go`), verify the delta — not `FinalPayload`
  — is still what gets applied onto a freshly re-read row; reverting to `FinalPayload` silently
  reintroduces Blocker 1 (a passing test suite alone won't catch it if the regression test itself
  gets weakened — the fix is confirmed by literally reverting it locally and observing the specific
  regression tests above fail, not just by reading the diff). The file-based fallback itself
  (decision 6/7: kept alive until a later phase retires it, together with the `$HOME/.boid`
  job-scoped tmpfs overlay that isolates it — see seam #13's PR6 update) is untouched by this PR and
  remains fully separate infrastructure; this seam is about the new RPC path only.

## 18. engine wait response → RuntimeExit → job status/diagnostics

Whether a docker `container.WaitResponse` the ENGINE could not fill in (`Error` set, `StatusCode`
left at its zero value — the runtime failed to wait, the shim died, the container never started) is
read correctly at the layer that owns exit-status *meaning* (`containerSession.waitLoop`), and
whether the layer that turns an exit code into job/task outcome (`Runner.watchRuntime`) still fails
the job even when the first layer's own check is bypassed or weakened. moby/moby/client's
`ContainerWait` forwards the response body verbatim without inspecting `Error`, so a caller reading
only `StatusCode` sees an engine failure as an ordinary "exited 0". The two ends currently overlap —
losing one does not (today) cause a false job success — which is exactly the shape of seam where a
later "the other end already covers it" edit removes the wrong one.

- **End A (produce — exit-code honesty)**: `containerSession.waitLoop`
  (`internal/dispatcher/container_backend.go`) and `containerBackend.RunWorkspaceInit`'s incumbent-
  wait path (`internal/dispatcher/container_backend_workspace_init.go`) both call the shared
  `waitResponseEngineError(res)` helper before trusting `res.StatusCode`. This is what keeps THIS
  layer's own `backend.RuntimeExit.ExitCode` honest, and what `NewDefaultDiagnosticsCollector`'s
  `exit.ExitCode != 0` gate (`internal/dispatcher/container_backend_diagnostics.go`) depends on to
  fire for an engine-side wait failure — the collector exists specifically to capture "the container
  didn't run right", and an unread `Error` used to make it skip exactly that case.
- **End B (consume — job/task outcome)**: `Runner.watchRuntime` (`internal/dispatcher/runner.go`)
  independently coerces a zero `RuntimeExit.ExitCode` to 1 and unconditionally sets
  `job.Status = JobStatusFailed` whenever a container exits without the in-container runner's own
  "job done" broker report ever arriving. A job is only ever marked successfully completed
  (`JobStatusCompleted`) via that separate in-container report path (`postJobDone` →
  `internal/server/boid_executor.go` → `Runner.CompleteJob`), which `watchRuntime` never touches — so
  End B, on its own, already prevents an engine-side wait failure from becoming a false "task done",
  independently of whatever End A does with `res.StatusCode`.
- **Invariant**: End A and End B currently each independently prevent a different bad outcome (End A:
  a dishonest exit code / a skipped diagnostics capture; End B: a job wrongly marked done) — do not
  assume either one's presence makes the other's redundant. **Past break**: `containerSession.waitLoop`
  read only `res.StatusCode` (missing End A) until PR #855 fixed it — this had NO consequence for job
  status or task completion (End B's unconditional-fail coercion already covered that independently),
  but it did mean `NewDefaultDiagnosticsCollector` silently never fired for an engine wait failure, and
  `job.Output`'s rendered `exit_code=%d` (from the unread, still-zero `result.ExitCode`) contradicted
  the `job.ExitCode` field `watchRuntime` had just forced to 1 in the same call.
- **Guard**: `TestContainerSession_WaitLoop_EngineErrorInTheWaitResponseFailsTheJob`
  (`internal/dispatcher/container_backend_test.go`) pins End A on the job dispatch path;
  `TestContainerBackend_RunWorkspaceInit_EngineErrorInTheWaitResponseFailsTheRun`
  (`container_backend_workspace_init_test.go`) pins it on the workspace-init path (which has no End B
  — a failed `RunWorkspaceInit` call is its own terminal outcome, not a job status). Neither test
  exercises End B's coercion directly, and there is no test today that pins "an engine wait error
  still fails the job even with End A's own check removed" — treat that as a known coverage gap, not
  as proof the two ends are redundant on purpose.
- **When you touch it**: if you touch `waitResponseEngineError`, either of its call sites, or
  `backend.RuntimeExit`'s shape, verify `NewDefaultDiagnosticsCollector`'s zero-check and
  `watchRuntime`'s 0→1 coercion still see the value you intend. If you ever remove or weaken
  `watchRuntime`'s unconditional-fail coercion (e.g. on the reasoning "waitLoop already reports engine
  errors correctly now, so this is redundant"), that is exactly the moment End A's check stops being
  cosmetic and starts being load-bearing for task-done correctness — add a test that pins the
  consequence before doing so, rather than relying on the other end having always been there.

## 19. config schema seam

Whether a new `internal/config/schema.go` leaf actually reaches the daemon behavior it's
meant to configure. Adding a config.yaml key touches a six-link chain — `Schema` leaf →
`Config` struct field → `Config.UnmarshalYAML` parse/validate → (if `ReloadRestartRequired`)
`restartFieldExtractors` entry → `cmd/start.go`'s `buildStartConfig` copying the resolved
value onto `server.Config` → whatever actually consumes that `server.Config` field. The
first four links are self-defending; the last two are not.

- **End A (schema ↔ extractor, self-defending)**: `config.Schema` (`internal/config/schema.go`),
  `Config`/`Config.UnmarshalYAML` (`internal/config/config.go`), and
  `restartFieldExtractors`/`restartFieldExtractorExemptions`
  (`internal/server/config_edit.go`). A `ReloadRestartRequired` leaf missing from both of the
  latter two maps makes `verifyRestartExtractorCoverage` **panic at daemon startup** (called
  from `wire.go`'s `buildRuntime`, before `GET/POST /api/config` can accept a single request) —
  and `TestRestartFieldExtractors_ExhaustiveCoverage`
  (`internal/server/config_edit_internal_test.go`) catches the same gap at `go test` time. A
  forgotten entry here cannot silently ship.
- **End B (buildStartConfig → consumer, NOT self-defending)**: `cmd/start.go`'s
  `buildStartConfig` reads the loaded `*config.Config` and copies each field it cares about
  onto the `server.Config` it returns (e.g. `cfg.HTTPAddr = appCfg.Web.HTTPAddr`,
  `cfg.LogLevel = appCfg.Log.Level`) — a plain, hand-written assignment per key, with **no
  compiler or test forcing every `Config` field to have one**. Nothing panics, nothing fails a
  build, and no generic coverage test exists if a future config key is added to `Schema` +
  `Config` + `restartFieldExtractors` (satisfying End A completely) but the
  `buildStartConfig`/`server.Config` copy is simply forgotten: `boid config set` accepts the
  key, `boid config get` shows it, the daemon even warns "requires restart" on change — and
  after that restart, the value never reaches whatever was supposed to consume it. The key
  looks fully wired end-to-end from every angle End A's forcing functions can see.
- **Invariant**: a `Schema` leaf that has an `internal/server/config_edit.go` extractor (or
  exemption) is not, by itself, evidence that the value reaches a live consumer — that
  requires a *second*, independent check on the `buildStartConfig`/`server.Config` half, which
  today only exists per-key (a dedicated test for that one field), never generically.
- **Past break**: none yet — flagged during PR #858 review (log.level) as a gap the PR itself
  happened to close correctly for its own key (see Guard below), not as an existing bug.
- **Guard**: End A has the generic, exhaustive tests named above. End B has **no generic
  test** — only per-key coverage as each key's own PR happens to add it, e.g.
  `TestBuildStartConfig_LogLevelFromConfig`/`TestBuildStartConfig_LogLevelUnset_Empty`
  (`cmd/start_test.go`) for `log.level`, or `TestBuildStartConfig_UsesDefaults`'s assertions on
  `cfg.HTTPAddr`/`cfg.AllowedDomains`/etc. for the older keys. A key added without its own
  `buildStartConfig`-level test would pass every existing test in the tree.
- **When you touch it**: when you add a new `config.yaml` key that a live daemon process is
  supposed to act on (not just persist), verify the chain past End A too — grep
  `buildStartConfig` for whether your key's `appCfg.*` value is actually copied onto the
  `server.Config` (or wherever else the daemon's runtime config lives) it returns, and add a
  test asserting that copy, the same way `TestBuildStartConfig_LogLevelFromConfig` does. Don't
  stop at "the schema/extractor tests pass" — that only proves the key round-trips through
  config.yaml, not that anything reads it.
- **Carve-out, confirmed while adding `oauth_providers` (PR2, docs/plans/api-gateway.md §6)**:
  not every `ReloadRestartRequired` key needs End B's `buildStartConfig`/`server.Config` copy
  at all — `gateway.forges`, `services`, and (as of PR2) `oauth_providers` are read straight
  off a SECOND, independent `*config.Config` (`internal/server/wire.go`'s own `boidCfg,
  err := config.Load()`, called well inside `buildRuntime` itself) at the exact point they're
  consumed (`gwCreds`/`apiGwCreds`/`apiGwCreds.SetOAuth2TokenSource`'s construction) — never
  through `cmd/start.go`'s `server.Config` at all. `services_floor` is the key that DOES need
  the End B copy (`cmd/start.go`: `cfg.ServicesFloor = appCfg.ServicesFloor`), because its
  actual consumer (`dispatcher.Runner.APIGatewayServicesFloor`, read by
  `resolveEnabledAPIServices` at dispatch time) is constructed earlier in `buildRuntime` from
  the `server.Config cfg` parameter, not from `boidCfg`. The distinguishing question when
  adding a new key: does its consumer already have direct access to a freshly-loaded
  `*config.Config` at the point it's built (no End B needed — verify by finding that
  construction site and confirming it reads `boidCfg.<YourField>` directly), or does it need
  to reach a component (`dispatcher.Runner`, `cmd/start.go`'s own `server.Config` return value)
  that was built from `cfg`/`appCfg` before `boidCfg` was loaded (End B needed)?

## 20. WorkspaceMeta write-path field coverage

Whether a `WorkspaceMeta` field a caller reads back after a write actually survived that
*specific* write path — `WorkspaceMeta` has **two independently-maintained write paths that
do not cover the same field set**, and nothing enforces they agree.

- **End A (strict PUT)**: `POST /api/workspaces` (create) and `PUT /api/workspaces/{slug}`
  (edit) decode the request body through `workspaceMetaStrict`
  (`internal/orchestrator/workspace_meta_strict.go`), then hand the result straight to
  `WorkspaceRepository.Create`/`UpdateIfRevisionMatches` — an **unconditional whole-row
  column write** from whatever that strict type decoded. `workspaceMetaStrict` does **not**
  declare `TaskBehaviors`/`BaseBranch`/`ForkPoint`/`DefaultTaskBehavior` at all (its own doc
  comment calls this out as a deliberate "PR3 scope boundary" that was never revisited), so a
  document decoded through this path always resolves those four fields to their Go zero value.
- **End B (envelope apply)**: `POST /api/workspaces/apply` decodes through
  `decodeWorkspaceEnvelopeSpec` into a `WorkspaceEnvelopeApply`, whose `FieldsPresent` map
  gates `MergeInto` per spec.* key — a key **absent** from the submitted document leaves the
  workspace's current value for that field completely untouched. This path *does* support all
  four fields End A is missing.
- **Invariant**: a caller that wants to change **one** `WorkspaceMeta` field while leaving
  every other field (in particular task_behaviors/base_branch/fork_point/
  default_task_behavior) untouched **must** go through End B with a minimal document (spec
  carrying only the key(s) it means to touch — built as a bare `map[string]any`, never by
  marshaling a `WorkspaceEnvelopeSpec` struct literal, since that type deliberately has no
  `omitempty` and would emit every other field as an explicit empty value, clearing them). A
  GET-full-meta-then-PUT-whole-meta-back round trip through End A will either 400 (if the
  fetched meta happens to carry a value in one of the four missing fields — the strict decoder
  rejects an unrecognized key under `KnownFields(true)`) or, were the decoder ever loosened to
  tolerate them, silently wipe those four fields on every such write.
- **Past break**: caught in review (not yet shipped) for `boid workspace services add/remove`
  (docs/plans/api-gateway.md PR1, `cmd/workspace_services.go`): the first cut fetched the full
  meta via GET, mutated only `Services`, and PUT the whole thing back through End A — confirmed
  empirically to 400 with `field task_behaviors not found in type
  orchestrator.workspaceMetaStrict` against a workspace that had `task_behaviors` set. Fixed by
  switching to End B with a `{"spec": {"services": [...]}}`-only document.
- **Guard**: `TestRunWorkspaceServices_Add_PreservesTaskBehaviorsAndBaseBranch`
  (`cmd/workspace_services_test.go`) pins the fix end-to-end. There is otherwise no generic
  test asserting End A and End B agree on field coverage — `workspaceMetaStrict`'s own
  "keep in sync with WorkspaceMeta" doc comment is aspirational prose, not an enforced
  invariant (contrast with `bareMetaKnownFieldNames`, which a **reflective** test,
  `TestBareMetaKnownFieldNames_CoversEveryWorkspaceMetaField`, does keep honest — that test
  only proves envelope detection recognizes the key, not that `workspaceMetaStrict` can carry
  its value).
- **When you touch it**: if you add a new CLI/API surface that reads a `WorkspaceMeta` field
  back and writes a single field in isolation, use End B (the envelope apply path) with a
  minimal spec document, not a GET-then-PUT round trip through End A — and if you add a new
  field to `WorkspaceMeta` itself, decide up front whether `workspaceMetaStrict` needs it too
  (most fields should be added to **both** decoders; the four PR3-era fields are the one
  known, deliberate exception, not a precedent to extend casually).

## 21. apigateway SecretResolver namespace threading

Sibling of seam #11, for `internal/apigateway` (docs/plans/api-gateway.md PR1). Same
concern, different gateway: whether a workspace-scoped secret namespace, chosen at
dispatch time, actually reaches the `SecretResolver` call that resolves the upstream
credential for a proxied request — namespace propagation across register → store →
recover → resolve, where any hop that drops the namespace silently collapses every
workspace back onto the `"default"` secret namespace.

- **End A (register)**: `Runner.registerAPIGatewayToken` in
  `internal/dispatcher/apigateway_wire.go` calls `r.APIGateway.Register(services,
  spec.SecretNamespace, spec.TaskID, readOnly)`.
- **End B (store)**: `apigateway.Registry.Register`/`RegisterToken`
  (`internal/apigateway/registry.go`) persist the namespace on `Entry.Namespace`.
- **End C (recover)**: `Server.ServeHTTP` (`internal/apigateway/server.go`) — a SINGLE
  `Registry.Lookup(rt.token)` call recovers the whole `Entry` (token validity, `Services`
  membership, `ReadOnly`, `Namespace`, `TaskID`) in one snapshot, which is stashed on the
  request-scoped `routeInfo` and read back by both the fail-fast `credentials.Resolve`
  pre-check and the `ReverseProxy.Rewrite` hook's `credentials.Inject` call. This is
  deliberately NOT `Registry.Authorize` (whose own internal `Lookup`) followed by a second,
  independent `Lookup` for the fields `Authorize`'s bool return doesn't expose — that
  two-call shape was PR1's original code and had a real TOCTOU: `Unregister` (job
  completion) landing in the window between the two calls handed the second `Lookup` a
  zero-value `Entry` (`ReadOnly=false`, `Namespace=""`) while `allowed` had already been
  computed `true` from the first, pre-race call — silently bypassing the read-only gate for
  a since-completed job's in-flight request AND mis-scoping credential resolution to the
  `"default"` namespace instead of erroring. Caught in review (codex, round 4) and fixed by
  collapsing to the single-Lookup shape above; `Registry.Authorize` itself is unchanged and
  still independently tested (`registry_test.go`) — it is simply no longer `Server.ServeHTTP`'s
  own call path.
- **End D (resolve)**: `apigateway.SecretResolver` (`func(namespace, key string) (string,
  error)`, `internal/apigateway/credentials.go`) — the closure built in
  `internal/server/wire.go` (`apiGwResolver`) passes `namespace` straight through to
  `secretStore.Get(namespace, key)`, which normalizes `""` to `"default"`
  (`dispatcher.SecretStore.normalizeNamespace`).
- **Invariant**: identical to seam #11's — the namespace a token was registered with
  (End A/B) is exactly the namespace `Resolve`/`Inject` resolve credentials against for
  every request authorized under that token (End C/D). No hop may substitute, drop, or
  hardcode a different namespace.
- **Past gap**: PR1 shipped with End D tested in isolation
  (`TestCredentialProvider_MultiNamespaceIsolation`, `internal/apigateway/
  credentials_test.go`) but no test proving End A→B (`spec.SecretNamespace` reaching
  `Entry.Namespace` through a real `Dispatch`) or End B→C→D (two tokens, two namespaces,
  routed to two different secrets through a real `Registry` + `Server`) — the exact two
  tests seam #11 already has for the sibling gateway. Caught in review (boid-review
  self-check before PR1's first push) by cross-referencing this catalog entry against the
  diff; fixed by adding both.
- **Guard**: `TestDispatch_RegistersAPIGatewayTokenWithSecretNamespace`
  (`internal/dispatcher/apigateway_wire_test.go`, End A→B) and
  `TestServer_RoutesCredentialsByTokenNamespace` (`internal/apigateway/server_test.go`,
  End B→C→D).
- **When you touch it**: if you touch `registerAPIGatewayToken`,
  `apigateway.Registry.Register`/`RegisterToken`, `Server.ServeHTTP`'s post-`Authorize`
  block, or the `apiGwResolver` closure in `internal/server/wire.go`, verify a token
  registered under namespace X still resolves credentials under namespace X — and keep this
  entry and #11 in sync if either gateway's namespace-threading shape changes (e.g. PR2's
  oauth2 TokenSource will need the same namespace to key its own token-cache lookup).
- **PR2 update (2026-08-03)**: confirmed — `CredentialProvider.Resolve`/`Inject`'s `AuthOAuth2`
  branch passes the SAME `namespace` parameter straight through to
  `c.oauth.AccessToken(namespace, rs.auth.Provider)` (`internal/apigateway/credentials.go`),
  and `OAuth2TokenSource.AccessToken`/`refresh` (`internal/apigateway/oauth2.go`) use that
  namespace, unmodified, both as half of every secret-store key it reads/writes
  (`OAuthSecretKey` is provider-only; the actual `resolver`/`writer` calls are
  `t.resolver(namespace, OAuthSecretKey(...))` / `t.writer(namespace, ...)`) and as half of
  the singleflight coalescing key (`namespace + "\x00" + provider`). Two workspaces sharing
  one `oauth_providers` entry therefore get fully independent refresh_token/access_token
  state and independent refresh coalescing — see
  `TestCredentialProvider_MultiNamespaceIsolation`-style coverage for the static kinds and
  `TestServer_ProxiesWithOAuth2Injection` (`internal/apigateway/server_test.go`) for the
  oauth2 kind's own End C/D round trip. See seam #23 for the surrounding config↔apigateway
  wiring PR2 also added.
- **Credential-accounts PR-1 update (2026-08-29)**: `docs/plans/
  api-gateway-credential-accounts.md` added a THIRD plain-string parameter (`account`) to
  both `CredentialProvider.Resolve` and `.Inject`, alongside `name` and `namespace` — three
  adjacent strings a call site can permute and still have it compile. `Server.ServeHTTP`'s
  own two call sites (the fail-fast pre-check and `Rewrite`'s inject) were changed to pass
  `namespace` FIRST: `Resolve(namespace, name, account string)` /
  `Inject(req *http.Request, namespace, name, account string)` — matching this package's
  own `SecretResolver(namespace, key string)` convention, and reducing (not eliminating —
  `name`/`account` are still adjacent same-typed strings) the chance of an accidental
  argument-order swap silently resolving one workspace's credential under a DIFFERENT
  workspace's namespace. Both call sites in `Server.ServeHTTP` and every test call site in
  `credentials_test.go` were updated together in the same change — the End C/D shape this
  entry documents is otherwise exactly what a partial reorder (only one of the two call
  sites) would break. `Resolve`'s own doc comment carries the same rationale, and (as of the
  PR-2 update below) is explicit that this stays a stopgap rather than getting resolved
  structurally.
- **Credential-accounts PR-2 update (2026-08-29)**: `Resolve`/`Inject`'s own signature did
  NOT change — `name`/`account` are still two adjacent bare strings, exactly as PR-1 left
  them; the PR-1 note above overpromised. What PR-2 actually did: `OAuth2TokenSource.
  AccessToken` (`internal/apigateway/oauth2.go`) changed from `(namespace, provider string)`
  to `(namespace string, cred credentialID)`, and `credentialID{provider: rs.auth.Provider,
  account: account}` is now assembled inline, once, inside `Resolve`/`Inject`'s `AuthOAuth2`
  case (`internal/apigateway/credentials.go`) right before that call — the only place either
  function builds one. `credentialID` never crosses back out as a `Resolve`/`Inject`
  parameter or return value; it exists purely to stop `oauth2.go`'s own internals (secret-store
  key, singleflight key, memCache key — see seam entry's `cred.secretPrefix()`/`cred.
  cacheKey()`) from re-deriving three different account-qualified strings by hand from
  separately-passed `provider`/`account` args. See seam #23 End D for the call site's exact
  current shape.

## 22. orchestrator.Action → timeline.Build renderability

Whether an `orchestrator.Action` a caller writes (via `TaskRepository.CreateAction`) is
actually visible anywhere a human looks — `timeline.Build` (`internal/timeline/timeline.go`)
is the **sole** function that turns `[]*Action` into what the Web UI's task-detail page
renders (`internal/api/web.go`'s `GET /tasks/{id}` route; the TUI that used to be the other
consumer was retired outright, `internal/tui` no longer exists), and its per-item filter is
narrower than "every Action row that exists":

```go
if !IsStateTransition(a) && !IsProgressAction(a) && !IsAPIGatewayRequestAction(a) {
    continue  // silently dropped — never becomes an Event in any StatusGroup
}
```

- **End A (writer)**: any `TaskRepository.CreateAction` call site that means for the row to
  show up in the timeline — `api.TaskAppService.NotifyTask`'s progress-mode branch
  (`internal/api/task_notify.go`), `internal/server`'s `newAPIGatewayRecorder`
  (`apigateway_notify.go`), and any future one.
- **End B (renderer)**: `timeline.Build`'s filter + `BuildActionLabel`
  (`internal/timeline/timeline.go`) — a `Type` this file doesn't recognize as a state
  transition, `"progress"`, or one of its own named non-transitioning kinds
  (`IsAPIGatewayRequestAction`, ...) never reaches an `Event`, no matter what its payload
  contains.
- **Invariant**: a **non-transitioning** Action (no real `FromStatus`→`ToStatus` state
  change — an FYI/audit row, not a lifecycle event) must set `FromStatus == ToStatus ==` the
  task's CURRENT status at write time (fetch it first if the caller doesn't already have it
  — `task.Status` right before `CreateAction`), AND its `Type` must be one `IsProgressAction`
  or an equivalent `Is*Action` predicate in `internal/timeline` recognizes. Missing either
  half breaks visibility a different way: a recognized `Type` with `FromStatus`/`ToStatus`
  left at their zero value opens a spurious empty-status `StatusGroup` nothing ever renders
  into or navigates to (group placement reads `a.FromStatus` unconditionally for every
  non-job item); an unrecognized `Type` (even with FromStatus/ToStatus correctly set) is
  filtered out before group placement ever runs.
- **Past break**: caught in review (not yet shipped) for the API gateway's request-timeline
  recording (docs/plans/api-gateway.md §論点3, `internal/server/apigateway_notify.go`): the
  first cut wrote `Action{TaskID, Type: "api_gateway_request", Payload}` with
  `FromStatus`/`ToStatus` left at their zero value. The row landed in the DB and every test
  that only asserted `ListActionsByTask` passed, but the feature the plan doc claims
  ("method + service + path + status を timeline に") was invisible on the actual Web UI
  page — confirmed empirically by feeding the exact Action `newAPIGatewayRecorder` produced
  through a real `timeline.Build` call and observing it in no `StatusGroup`'s `Events`.
  Fixed by (a) fetching the task and setting `FromStatus = ToStatus = task.Status` before
  `CreateAction`, and (b) adding `timeline.IsAPIGatewayRequestAction` +
  `timeline.ActionTypeAPIGatewayRequest` (exported so the writer and the two readers share
  one string, not two hand-copied literals) and wiring it into `Build`'s filter and
  `BuildActionLabel`.
- **Guard**: `TestNewAPIGatewayRecorder_ActionIsVisibleInTimeline`
  (`internal/server/apigateway_notify_test.go`) round-trips a recorded Action through a real
  `timeline.Build` and asserts it appears with the expected label — the class of test this
  seam needs (not merely "the DB row has the right Type/Payload", which the same file's
  other tests already covered and which would NOT have caught this).
- **When you touch it**: if you add a new kind of non-transitioning `Action` meant to show up
  in a task's timeline, (1) give it its own exported `ActionType*` constant + `Is*Action`
  predicate in `internal/timeline` (don't just reuse `"progress"` if the payload shape
  differs — `buildProgressLabel` expects a `message` key and silently degrades to a bare
  "進捗" for anything else), (2) wire the predicate into `Build`'s filter AND
  `BuildActionLabel`, and (3) set `FromStatus`/`ToStatus` to the task's current status at
  write time. Write a test that calls `timeline.Build` on the real Action your writer
  produces and asserts it surfaces as an `Event` — a test that only inspects
  `ListActionsByTask`'s return value proves the DB write, not the feature.

## 23. oauth_providers config↔apigateway wiring

Whether `config.yaml`'s `oauth_providers:` block (docs/plans/api-gateway.md §6/§論点4, PR2)
actually reaches a live `OAuth2TokenSource` a `CredentialProvider` will call — a **type
mirror** (like seam #7's `CommandDef`) plus a **conditional wiring step** (like seam #21's
resolver closures) chained together, spanning `internal/config` → `internal/server/wire.go` →
`internal/apigateway`.

- **End A (config schema)**: `config.OAuthProviderConfig` (`internal/config/apigateway.go`,
  yaml-tagged fields `token_endpoint`/`client_id`/`client_secret_key`/`scopes`), validated by
  `validateOAuthProviderConfig` and parsed into `Config.OAuthProviders` by
  `Config.UnmarshalYAML`. Mirrored by `schema.go`'s four `oauth_providers.*.*` `Schema`
  entries + `IsOAuthProviderEntryPath` (wired into `dotted.go`'s `Get`/`Unset`) + `internal/
  server/config_edit.go`'s `changedOAuthProviderLeaves` and the matching
  `restartFieldExtractorExemptions` quartet — this half is seam #19's "End A", already
  self-defending via `verifyRestartExtractorCoverage`/`TestRestartFieldExtractors_
  ExhaustiveCoverage`, and does NOT need seam #19's End B (`buildStartConfig`/`server.Config`)
  — see #19's PR2 carve-out note for why.
- **End B (type mirror)**: `Config.APIGatewayOAuthProviders()` converts the config-package
  shape into `apigateway.OAuthProviderConfig` (`internal/apigateway/oauth2.go`, same four
  fields plus `Name`) — a hand-written field-by-field copy with no compiler check that every
  field actually made the trip. A field added to one struct's yaml/CredentialProvider surface
  without a matching field (and a matching copy line in `APIGatewayOAuthProviders`) on the
  other silently drops that field's config value the moment it's used at runtime.
- **End C (construction + conditional wiring)**: `internal/server/wire.go`'s `buildRuntime` —
  `apigateway.NewOAuth2TokenSource(boidCfg.APIGatewayOAuthProviders(), apiGwResolver,
  apiGwWriter)`, called (and `apiGwCreds.SetOAuth2TokenSource` invoked) only `if secretStore !=
  nil`, mirroring the exact `apiGwResolver`/`gwResolver` "nil resolver means unconfigured, not
  a build-time absence" convention seam #21 already established — a `CredentialProvider` never
  reaches this call at all when `secretStore == nil`, and `SetOAuth2TokenSource`'s own doc
  comment (`internal/apigateway/credentials.go`) is explicit that a never-called setter leaves
  `oauth == nil`, which `Resolve`/`Inject` treat as a normal (loud, non-panicking) error case,
  not a nil-deref crash.
- **End D (consumer)**: `CredentialProvider.Resolve`/`Inject`'s `AuthOAuth2` branch
  (`internal/apigateway/credentials.go`) builds `cred := credentialID{provider:
  rs.auth.Provider, account: account}` and calls `c.oauth.AccessToken(namespace, cred)`
  (credential-accounts PR-2, docs/plans/api-gateway-credential-accounts.md §3 — see seam #21's
  PR-2 update note; `AccessToken`'s signature used to be `(namespace, provider string)`
  before that PR). `rs.auth.Provider` (from `services.<name>.auth.provider`, `cred.provider`
  here) is looked up against `OAuth2TokenSource.providers`, itself keyed by
  `OAuthProviderConfig.Name` from End B/C — `cred.account` plays no part in that lookup, only
  in the secret-store/cache keys `AccessToken` derives internally.
  A `provider` string that doesn't match any `oauth_providers.<name>` key produces a clear
  "oauth2 provider %q is not configured" error (`oauth2.go`'s `AccessToken`) — deliberately NOT
  a config-load failure (see `TestLoadFromPath_Services_OAuth2ProviderReferenceNotCrossValidated`,
  `internal/config/oauth_providers_test.go`, for why that cross-reference is left
  request-time-only, matching every other secret-store-shaped reference in that package).
- **Invariant**: every field of `config.OAuthProviderConfig` that has runtime meaning must
  (1) have a corresponding field on `apigateway.OAuthProviderConfig`, (2) be copied by
  `APIGatewayOAuthProviders`, and (3) actually be read by `OAuth2TokenSource` where relevant —
  and a `CredentialProvider`'s `oauth` field must be non-nil in every runtime configuration
  where `secretStore` is configured (i.e. wherever the static `bearer`/`basic`/`header`/`query`
  kinds already work), never just in the code paths a test happens to exercise.
- **Past break**: caught in review (boid-review self-check before PR2's first push): the
  initial `TestAPIGatewayOAuthProviders_SortedByName`-style test for End B only asserted on
  `Name`/ordering (mirroring `TestAPIGatewayServices_SortedByName`'s identical, pre-existing
  scope for the sibling `services` conversion) — which would not have caught a dropped or
  swapped `TokenEndpoint`/`ClientID`/`ClientSecretKey`/`Scopes` field. Fixed by adding
  `TestAPIGatewayOAuthProviders_FieldMapping`, which asserts every field individually.
- **Guard**: `TestAPIGatewayOAuthProviders_FieldMapping` (End A→B field-by-field),
  `TestServer_ProxiesWithOAuth2Injection` (`internal/apigateway/server_test.go`, End B→C→D
  through a real `Server`+`Registry`+`CredentialProvider`+`OAuth2TokenSource`+fake token
  endpoint, proving the upstream actually receives a Bearer token the test never gave it
  directly), and the `oauth2.go`-focused unit tests in `oauth2_test.go` (End D's
  `AccessToken`/refresh/persistence-order behavior in isolation).
- **When you touch it**: if you add a field to `config.OAuthProviderConfig` or
  `apigateway.OAuthProviderConfig`, add the matching field to the other struct AND a copy line
  in `APIGatewayOAuthProviders`, then extend `TestAPIGatewayOAuthProviders_FieldMapping` to
  cover it — don't rely on a `SortedByName`-style test to catch a dropped field. If you change
  the `secretStore != nil` gating in `wire.go`, verify (with a test) that `oauth2`-kind
  services still fail loud (not nil-deref) when unconfigured, and still resolve when
  configured — `TestCredentialProvider_Inject_OAuth2NoTokenSourceConfigured` and
  `TestServer_ProxiesWithOAuth2Injection` are the two ends of that check today.
- **PR3 extension (login flow, docs/plans/api-gateway.md §7)**: four more fields joined this
  same mirror — `Flow`/`AuthorizationEndpoint`/`DeviceAuthorizationEndpoint`/`AuthorizeParams`
  — on both `config.OAuthProviderConfig` and `apigateway.OAuthProviderConfig`, copied by the
  same `APIGatewayOAuthProviders`, and `TestAPIGatewayOAuthProviders_FieldMapping` was extended
  to cover all four. Unlike the PR2 quartet, `Flow` is **optional** (`""` is valid — an existing
  PR2-era config.yaml with no `flow` at all must keep loading unmodified;
  `TestLoadFromPath_OAuthProviders_FlowOptional` pins this), so this seam's End A validation
  (`validateOAuthProviderConfig`) only enforces the flow-conditional endpoint fields when `Flow`
  is actually set. See seam #24 below for the NEW three-tier wiring PR3 added on top of this one
  (the login-session HTTP surface) — that is a distinct seam from this type-mirror, not an
  extension of it.

## 24. OAuthLoginHandler ↔ apiGatewayLoginAdapter ↔ apigateway.LoginManager

Whether `boid secret oauth login <service>` (docs/plans/api-gateway.md §7, PR3) actually
reaches a live `apigateway.LoginManager` session and whether that session's user-facing
fields survive translation across THREE packages: `internal/apigateway` (the real
device/loopback/manual logic) → `internal/server` (a translation adapter) →
`internal/api` (the wire-facing DTOs and chi routes) → `cmd` (the CLI's own duplicated wire
DTOs, per the `cmd/login.go` precedent of not importing server-internal unexported types).

- **End A (real logic)**: `apigateway.LoginManager` (`internal/apigateway/login.go`) —
  `StartLogin`/`CompleteLogin`/`Status`, returning `*apigateway.LoginStart` /
  `apigateway.LoginStatus`.
- **End B (translation adapter)**: `apiGatewayLoginAdapter`
  (`internal/server/apigateway_login.go`) — a HAND-WRITTEN field-by-field copy from
  `apigateway.LoginStart` into `api.OAuthLoginStart` (internal/api's OWN DTO, not an import
  of the apigateway type — internal/api must not import internal/apigateway's login surface
  as a wire contract, mirroring the `internal/client` "type import ok, behavior import not"
  discipline applied one layer earlier). A field added to `apigateway.LoginStart` without a
  matching field on `api.OAuthLoginStart` AND a copy line in `apiGatewayLoginAdapter.StartLogin`
  silently drops that field from the CLI's response the moment it's used at runtime — the
  identical failure shape as seam #23's End A→B.
- **End C (HTTP handler)**: `api.OAuthLoginHandler` (`internal/api/oauth_login.go`) — mounts
  `POST/GET /api/oauth/login*` onto `api.OAuthLoginService` (the interface `apiGatewayLoginAdapter`
  implements), JSON-(de)serializing `oauthLoginStartRequest`/`oauthLoginStartResponse`/
  `oauthLoginStatusResponse`/`oauthLoginCompleteRequest`.
- **End D (wiring gate)**: `internal/server/wire.go`'s `buildRuntime` — `oauthLoginSvc` (and
  therefore the whole `/api/oauth` mount in `mountRoutes`) is non-nil ONLY when
  `oauthTokenSource != nil` (i.e. `secretStore` configured), mirroring seam #23 End C's exact
  same `secretStore != nil` gating convention — a request against `/api/oauth/*` 404s at the
  router level (route never mounted) rather than reaching a nil-deref when unconfigured.
- **End E (CLI wire DTOs)**: `cmd/secret_oauth.go`'s `oauthLoginStartRequest`/
  `oauthLoginStartResponse`/`oauthLoginStatusResponse`/`oauthLoginCompleteRequest` — DUPLICATED
  (not imported) from `internal/api/oauth_login.go`'s identically-named-but-unexported request/
  response structs, the same `cmd/login.go` `deviceAuthRequest`/`deviceAuthResponse` precedent
  (that file's own doc comment gives the full rationale: the server-side types are deliberately
  unexported, since that handler is server-internal, not a shared client/server contract
  package). A JSON field renamed on the End C struct without the identical rename on this one
  silently breaks the CLI (a field either vanishes on decode, or double-decodes as its zero
  value) with NO compiler error on either side — struct tags are strings, not types.
- **Invariant**: every field that must reach the CLI (`SessionID`/`Flow`/`Account`/
  `AuthorizeURL`/`UserCode`/`VerificationURI`/`VerificationURIComplete`/`IntervalSeconds`/
  `ExpiresInSeconds`) must be present, correctly JSON-tagged, and correctly copied at EVERY one
  of the five ends above — a `SortedByName`/`Name`-only-style test at any single hop would not
  catch a dropped field the same way seam #23's own past break didn't. This is NOT
  response-direction-only: `oauthLoginStartRequest.Account` (End C, `internal/api/
  oauth_login.go`) and its DUPLICATED mirror on `cmd/secret_oauth.go`'s own
  `oauthLoginStartRequest` (End E) must carry the identical `json:"account,omitempty"` tag —
  renaming one side's tag compiles cleanly on BOTH sides (struct tags are strings, not types,
  the exact same failure shape already described above for the response struct) and makes
  `--account` silently do nothing: the field just fails to decode server-side, StartLogin runs
  an ordinary unqualified login, and no error surfaces anywhere in the round trip.
- **Past break**: caught in review (boid-review self-check before PR3's first push): End B
  (`apiGatewayLoginAdapter`) shipped with ZERO test coverage at all — `internal/api/
  oauth_login_test.go` only exercises End C against a HAND-ROLLED FAKE `OAuthLoginService`
  (never the real adapter), so a translation bug in `apiGatewayLoginAdapter.StartLogin` (e.g. a
  dropped or swapped field) would have passed every existing test in the tree. Fixed by adding
  `internal/server/apigateway_login_test.go`, which builds a REAL
  `apigateway.CredentialProvider` + `apigateway.LoginManager` (with a fake token endpoint) and
  drives the adapter directly — `TestAPIGatewayLoginAdapter_StartLogin_FieldMapping` in
  particular, mirroring `TestAPIGatewayOAuthProviders_FieldMapping`'s own "field-by-field, not
  just Name/ordering" lesson at this new hop.
- **Guard**: `internal/apigateway/login_test.go` (End A in isolation), `internal/server/
  apigateway_login_test.go` (End A→B, the hop none of `internal/apigateway`'s own tests can
  reach — same rationale as `apigateway_notify_test.go`'s `TestNewAPIGatewayRecorder_...`),
  `internal/api/oauth_login_test.go` (End C against a fake `OAuthLoginService` — request
  validation, status-code mapping, NOT the real adapter), `cmd/secret_oauth_test.go` (End C→E,
  the CLI's three flow branches against a fake HTTP daemon).
- **A second, narrower hazard found in the SAME review pass**: `apigateway.LoginManager.
  StartLogin`'s device-flow arm (`startDevice`) unconditionally spawns a background
  `pollDeviceGrant` goroutine that keeps polling `TokenEndpoint` until the grant
  succeeds/fails/expires — ENTIRELY independent of whether the calling test (or a real CLI
  invocation) ever observes that outcome. A test that calls `StartLogin` for a device-flow
  provider and returns without waiting for a terminal `Status` leaves that goroutine running
  against whatever `TokenEndpoint` string the test configured for the rest of the test binary's
  process lifetime — caught concretely when a first draft of
  `TestLoginManager_StartDevice_SendsScopesClientIDAndAuthorizeParams` used the literal
  `"https://example.com/token"` as a lazy placeholder never expected to be dialed: the leaked
  goroutine actually reached the real (live, third-party) example.com host and logged a stray,
  test-run-polluting 405 minutes into an unrelated later test's `-v` output, from unlucky
  timing between the goroutine's unbuffered `slog` write and the `testing` package's own
  per-test output flushing (not a logical association with whichever test happened to be
  printing at that moment). No assertion ever failed — this is a "silent hazard", not a broken
  test.
- **When you touch it**: (1) if you add a field to `apigateway.LoginStart`, update
  `api.OAuthLoginStart`, `apiGatewayLoginAdapter.StartLogin`'s copy, `oauthLoginStartResponse`
  in BOTH `internal/api/oauth_login.go` and `cmd/secret_oauth.go`, and
  `TestAPIGatewayLoginAdapter_StartLogin_FieldMapping` — five places, not two. (2) any test that
  calls `LoginManager.StartLogin` for a device-flow provider must either wait for a terminal
  `Status` (`waitForStatus` in `login_test.go`) before returning, or use an
  `ExpiresInSeconds`/ticker `interval` short enough that the goroutine self-terminates almost
  immediately — never leave one running against a placeholder that could resolve to a REAL
  host on the network.
- **Credential-accounts PR-3 update (2026-08-30)**: `docs/plans/
  api-gateway-credential-accounts.md` D9 (`boid secret oauth login --account`) touched all FIVE
  ends this entry documents, in both directions:
  - `LoginManager.StartLogin`'s signature (End A) grew a fourth plain-string parameter:
    `(namespace, provider, redirectURI, account string)` — four adjacent, same-typed strings a
    call site can permute and still have it compile, the identical hazard seam #21's
    Credential-accounts PR-1 update already called out for `Resolve`/`Inject`'s three adjacent
    strings (`namespace, name, account`). `api.OAuthLoginService.StartLogin` (End C's interface)
    and `apiGatewayLoginAdapter.StartLogin` (End B) mirror the same four-parameter order, so a
    reorder at any one of these three call sites (End A/B/C) would silently swap `redirectURI`
    and `account` at runtime with no compiler error — `redirectURI` is ignored for
    device/manual flows and `account` is validated (non-empty must match `[A-Za-z0-9_-]{1,64}`),
    so the failure would likely surface as a confusing validation error on `redirectURI`'s value
    rather than a clean type mismatch, making it easy to misdiagnose.
  - Request DIRECTION also gained a duplicated field for the first time on this seam:
    `oauthLoginStartRequest.Account`, independently declared on both End C
    (`internal/api/oauth_login.go`) and End E (`cmd/secret_oauth.go`) — see the Invariant note
    above for why a JSON-tag rename on only one side is a silent, compiler-invisible break, the
    same shape as every RESPONSE field this entry already tracked, just running the other way.
  - Item 2 of this same review round (docs/plans/api-gateway-credential-accounts.md) added
    `Account` to the RESPONSE side too — `apigateway.LoginStart.Account` (End A) through
    `api.OAuthLoginStart.Account` (End B) and both `oauthLoginStartResponse.Account` structs
    (End C/End E) — specifically so the CLI (End E) can detect a daemon (End A-D) old enough to
    silently ignore `--account` altogether (an old daemon's request DTO has no `account` field
    to decode into, so `StartLogin` there runs an ordinary unqualified login and this new
    response field always comes back `""` regardless of what the CLI asked for). This is the
    same "five places, not two" list the "When you touch it" bullet above already gives.
  - **Guard extension**: `TestAPIGatewayLoginAdapter_StartLogin_AccountFieldMapping`
    (`internal/server/apigateway_login_test.go`, End A→B for `Account` specifically — the
    existing `_FieldMapping` test only asserted `Account == ""` for an unqualified call and
    would not have caught a dropped/swapped `Account` copy line on its own),
    `TestOAuthLoginHandler_Start_ResponseEchoesAccount` (`internal/api/oauth_login_test.go`, End
    C's response-side wire encoding), `TestSecretOAuthLogin_AccountEchoMismatch_
    AbortsBeforeComplete`/`TestSecretOAuthLogin_AccountEchoMatch_Proceeds`/
    `TestSecretOAuthLogin_NoAccountFlag_NoEchoMismatchFalsePositive` (`cmd/secret_oauth_test.go`,
    End E's actual version-skew-detection behavior — not just wire encoding, the CLI's real
    abort-before-any-flow-dispatch decision), and `TestLoginManager_CompleteLogin_Manual_
    WithAccount_PriorReadIsAccountQualified` /
    `TestLoginManager_StartDevice_WithAccount_PriorReadIsAccountQualified`
    (`internal/apigateway/login_test.go`, End A only — pins that `exchangeAndPersist`'s and
    `pollDeviceGrant`'s OWN, independent `priorRefreshToken` reads stay keyed by the
    account-qualified credential; a regression to the unqualified `cfg.Name` key here survives
    every pre-existing test in the tree, since none of them seed the SAME refresh_token value
    under the unqualified key first).

## 25. orchestrator.Action.Actor ctx propagation

Whether an `Action` write correctly attributes WHO/WHAT drove it — `orchestrator.ActorHuman` /
`ActorDaemon` / `ActorTask(taskID)` — depends on a ctx-carried value that has NO compiler link
between where it's set and where it's read, the same shape of hazard as seam #22 but one level
upstream (this seam is about `Action.Actor` being *correct*; #22 is about the write being
*visible* once made).

- **End A (origin, sets ctx)**: every entry point that can determine who/what is really behind
  a call — `internal/api/action.go`'s `Apply` (HTTP), `internal/api/web_service.go`'s
  `ApplyAction`/`Wake`/`ReopenTask` (Web UI), `internal/api/task.go`'s `Answer` /
  `internal/api/web.go`'s `PostAnswer`-equivalent (both `ActorHuman`), and every
  `internal/server/boid_executor.go` op that calls into `ApplyAction`/`Wake`/`AnswerTask`
  (`ActorTask(ctx.TaskID)` — `ctx` here is `sandbox.TokenContext`, the CALLING task, not
  necessarily the task the write lands on). Daemon-internal callers with no real ctx to carry
  (`workflow_action.go`'s `auto_advance`/`dispatch_error`/`abort`/`persistFiredEvents`,
  `queue_sweep.go`'s `SweepWake`, `dispatcher/store.go`'s `markStaleTasksAborted`,
  `workflow_card.go`'s `recordChildClosedOnParent`) hardcode `orchestrator.ActorDaemon`
  directly instead of relying on an (absent) wrapped ctx.
- **End B (construction, reads ctx)**: every `&orchestrator.Action{...}` literal deep in a
  shared call chain — `ApplyAction` (`workflow_action.go`), `Wake`/`Dispatch`
  (`workflow_card.go`) — reads `orchestrator.ActorFromContext(ctx)`. A ctx that flows through
  `ApplyAction`/`Wake`/`Dispatch` WITHOUT ever passing through `orchestrator.WithActor` at any
  End A silently produces `Actor: ""` — not a compile error, not even a test failure unless a
  test specifically asserts the actor.
- **Invariant**: every NEW brokered `boid_executor.go` op or NEW `TaskAppService`/
  `TaskWorkflowService` method that can be reached from more than one kind of caller (human
  HTTP/Web/CLI vs. brokered sandbox vs. daemon-internal loop) must either accept `ctx` and wrap
  it with the correct `WithActor` at every one of ITS OWN origins, or hardcode the one actor
  value that's actually always true for every path that reaches it — never leave a shared
  ctx-accepting function silently defaulting to `""`.
- **Past break**: caught in review (Opus, before merge) on the PR that introduced this seam
  (`feat/action-actor-field`, PR #935): `boid_executor.go`'s `BoidOpTaskWake` case called
  `e.workflow.Wake(context.Background(), ...)` with a bare, unwrapped ctx — its two sibling
  cases in the same switch (`BoidOpActionSend`, `BoidOpTaskReopen`) DID wrap
  `ActorTask(ctx.TaskID)`, so the omission wasn't visible by pattern-matching the file, only by
  checking every case individually. Two more of the same shape: `queue_sweep.go`'s `SweepWake`
  (the periodic machine-driven wake — the one path this field most needs to distinguish from a
  human pressing Wake — passed its bare loop ctx straight into `Wake`), and
  `workflow_card.go`'s `recordChildClosedOnParent`/the `child_dispatched` write inside
  `Dispatch` (one used no Actor at all, the other diverged from the sibling `"dispatch"` Action
  written in the SAME transaction). A fourth class: `task_ask.go`'s `recordAnswerAction`
  hardcoded `ActorHuman` even though `AnswerTask` is reachable from `boid_executor.go`'s
  `BoidOpTaskAnswer` (a supervisor task answering its own child) — fixed by threading `ctx`
  through `answerBlocking` and, for the durable-park slow path, adding
  `AwaitingPayload.PendingAnswerActor` so the actor survives the disconnect gap and
  `consumePendingAnswer` attributes the eventual action to the ORIGINAL answerer, not to the
  agent's later re-ask call.
- **Guard**: `TestTaskWorkflowServiceApplyAction_StampsActorFromContext`
  (`internal/api/apply_action_phase1_test.go`, human/task/unset), `TestBoidBuiltinExecutor_
  TaskWake_StampsActorTask` / `TestBoidBuiltinExecutor_ActionSend_StampsActorTask`
  (`internal/server/boid_executor_task_wake_test.go`), `TestSweepWake_StampsActorDaemon`
  (`internal/api/queue_sweep_test.go`), `TestFinalizeTerminal_ChildClosed_SelfRecordsOnParent`'s
  actor assertion (`internal/api/apply_action_pr4_test.go`), and
  `TestAnswerTask_StampsActorFromContext` (`internal/api/task_ask_test.go`) — each drives the
  REAL op/method end-to-end and asserts the recorded `Action.Actor`, not just that ctx-wrapping
  code exists somewhere.
- **When you touch it**: adding a new `boid_executor.go` op, a new `TaskAppService`/
  `TaskWorkflowService` entry point, or a new daemon-internal periodic loop that writes an
  `Action` — grep every existing case in the same switch/file for its `WithActor`/`ActorDaemon`
  pattern before assuming yours doesn't need one, and add a test asserting the actor, not just
  the action `Type`/status transition.
- **Behavioral read added (2026-08-19, docs/plans/ingestion-identity.md PR-5, I-5 auto-reopen)**:
  until this PR, every reader of `Action.Actor` was AUDIT-ONLY — a wrong value made a log/timeline
  entry misleading, nothing more. `orchestrator.CountAutoReopens` (`internal/orchestrator/
  auto_reopen.go`) is the first BEHAVIORAL reader: it filters a task's own action history for
  `Type=="reopen_triaged" && Actor==ActorDaemon` (scoped to the task's current done episode — see
  the function's own doc comment) to decide whether `SweepReopen`'s フラップ対策 fires. A
  mis-attributed Actor here doesn't just dirty a log line — it directly mis-spends or
  under-spends the フラップ budget: an `ActorDaemon` reopen wrongly stamped `ActorHuman` (or vice
  versa via a future new caller of `reopen_triaged`) changes whether the NEXT flip auto-reopens
  or gets silently blocked/notified-only. Any future code path that writes a `reopen_triaged`
  Action — or a `triage_done`/`done` Action, since those bound the episode boundary
  (`isDoneEntryAction`) — inherits this seam's existing origin/construction discipline (End
  A/End B above) with higher stakes than before: get the Actor wrong here and the bug is a
  behavior change (budget consumed or not), not just an audit-trail cosmetic.

---

## 26. brokered op scoping layer (broker request-shaping ↔ executor re-check)

Which LAYER enforces a brokered op's workspace scoping. Getting this wrong doesn't fail to
compile and doesn't fail any test that only drives the happy path — it silently opens a
cross-workspace read.

- **End A (broker, `internal/sandbox/broker.go`'s per-op `switch`)**: **shapes and gates the
  request** before it ever reaches the daemon. For the list-shaped ops this is where the real
  work happens: `resolveProjectRef` turns a project **name** into a UUID, `AllowsProject`
  checks it, a caller-supplied `WorkspaceID` is compared against `entry.Context.WorkspaceID`
  (no escape hatch), and an unfiltered request gets the caller's own workspace **injected**.
- **End B (executor, `internal/server/boid_executor.go`)**: re-checks `ctx.AllowsProject` for
  ops that name a single task/project. This is defense-in-depth for a handwritten request, NOT
  the primary gate — and it can only check what it can see: a `WorkspaceID` filter or an
  unresolved project **name** is meaningless to `AllowsProject`, which compares UUIDs.
- **Invariant**: every caller-supplied selector that widens the result set (`project_id`,
  `workspace_id`) is validated where it is *resolvable* — project refs and workspace ids in the
  broker, per-task project ids in the executor. `orchestrator.TaskFilter.WorkspaceID` is
  implemented as an `INNER JOIN project_workspaces … workspace_id = ?`, so it genuinely crosses
  the compartment; an unchecked value is a disclosure, not a cosmetic bug.
- **Past break**: PR-5a's `BoidOpTaskTriageList` (caught in review, not shipped). It was written
  as "scoping mirrors `BoidOpTaskList`" but placed **all** of it in the executor, because the
  author assumed task_list's scoping lived there. Two defects at once: `--workspace-id <other>`
  returned every other workspace's triage cards (ids, titles, urgency, and the whole `detail`
  blob — summary/source/children/observed), and `--project-id <name>` was unusable because a
  name never matches `AllowsProject`'s UUID space. Same class as PR-4 round 1's two Blockers
  (`child_specced`'s `project` field bypassing workspace authorization; root-task ref dedup
  being workspace-unscoped) — **three consecutive PRs in this plan hit "daemon-side
  permission/scope check missed"**.
- **Guard**: `TestBroker_BoidTaskTriageList_WorkspaceIDMismatchDenied` /
  `_ProjectIDOutsideWorkspaceDenied` / `_UnfilteredInjectsOwnWorkspace`
  (`internal/sandbox/broker_test.go`), mirroring `TestBroker_BoidTaskList_*`; plus the
  executor-side `TestBoidBuiltinExecutor_TaskTriage*` cross-workspace tests for End B.
- **When you touch it**: adding a brokered op that takes a `project_id` or `workspace_id`
  filter — **read the op you are mirroring and find where its checks physically are** before
  writing "scoped like X". Then write the denial test at that layer: a test that only proves
  the allowed case passes proves nothing about the seam.

## 27. park rule set ↔ Wake origin resolution

Which task statuses can be "parked" is declared entirely on one side of the codebase; which
parked-origins can be "woken" is resolved entirely on the other. Nothing at the type level
forces them to stay the same set.

- **End A (`internal/orchestrator/machine.go`'s `NewMachine`)**: the set of
  `{Action: "park", FromStatus: X, ToStatus: "parked", Manual: true}` rules. Each one declares a
  status a task can be parked FROM. A matching `{Action: "wake_X", FromStatus: "parked",
  ToStatus: X}` rule (Manual:false — see the doc comment above `NewMachine`) is what makes that
  origin recoverable.
- **End B (`internal/api/workflow_card.go`'s `TaskWorkflowService.Wake`)**: reads
  `CardAttrs.ParkedFrom` (derived from the actions log, not a stored column) and `switch`es on
  it to pick which `wake_X` action to apply. This switch is a **hand-maintained mirror** of End
  A's origin set — it has no way to discover new origins from the rule table itself, since
  `Wake` calls `sm.Apply` with a resolved action name it already decided on, not a generic
  "wake this origin" lookup.
- **Invariant**: the set of `FromStatus` values across all `park` rules in `NewMachine` must
  equal the set of `case` values (mapped through their `wake_X` counterpart) in `Wake`'s
  `ParkedFrom` switch. A park origin with no matching `wake_X` rule AND switch case can be
  parked but never woken.
- **Past break (BD-9)**: Phase 1 PR-4 (論点8) added a third park rule, `park: working →
  parked`, to let a dispatched task be parked mid-flight and re-surface later via
  `wake_task_id` + `SweepWake` (the "sequential PR consumption" pattern). PR-3's `Wake`,
  written before that rule existed, only had `case triaged` / `case ready` — no state-machine
  rule named `wake_working` existed either. A working-origin park's `Wake` call fell into the
  `default:` branch and 500'd (`wake: unexpected park origin "working"`). Worse, the periodic
  `SweepWake` (`internal/api/queue_sweep.go`) that drives the `wake_task_id` re-surface path
  swallows `Wake`'s error into a `slog.Warn`, so the failure never surfaced anywhere a human
  would see it — a real khi workspace card (`633c4bd9-1e6e-476b-9559-052e32882945`) sat
  permanently un-wakeable, invisible in every Web UI tab, until traced from raw API state.
- **Guard**: `TestDefaultMachine_ParkOrigins_AllHaveWakeRule` (`internal/orchestrator/
  machine_test.go`) derives the park-origin set from `DefaultMachine().Rules` itself (never a
  hardcoded literal) and asserts a matching `parked → origin` rule exists for each one.
  `TestTaskWorkflowService_Wake_ResolvesAllParkOrigins` (`internal/api/
  apply_action_phase1_test.go`) does the same derivation and end-to-end pins that `Wake`
  resolves every derived origin without error. Together these fail by name the next time a park
  rule is added without a matching wake rule/case, instead of waiting for a production 500.
  (Named single-origin pins — `TestDefaultMachine_WakeWorking`,
  `TestTaskWorkflowService_Wake_FromWorking_ReturnsToWorking_NoDispatch`, etc. — still exist
  alongside these and remain useful for origin-specific behavior like the Dispatch-chain
  skip, but do not by themselves catch a *new*, as-yet-unnamed origin.)
- **When you touch it**: adding or removing a `park: X → parked` rule in `machine.go` — add (or
  remove) the matching `wake_X: parked → X` rule in the same commit, add the matching `case` in
  `Wake`'s `ParkedFrom` switch, and confirm the two derived guard tests above still pass (they
  will fail by name if you forget either half).

## 28. status tab literal ↔ ListTasks status predicate — REMOVED (PR-4, 2026-08-26)

This seam described a Web UI status *tab* (`web/templates/components/filters.templ`'s
`statusTab`) wired to `orchestrator.ListTasks`'s status filter by two independently hand-typed
string literals, plus a matching `internal/api/web.go` `case filter.Status == "parked":` branch
that picked `BuildTreeItemsWithSuggestions` over the default tree builder. `docs/plans/
webui-detail-list-redesign.md` PR-4 (#999, 2026-08-25/26) deleted the entire Open/Closed/Queue/
Parked tab switcher (`statusTab`/`statusTabActive`), the tree/queue builder family
(`BuildTreeItemsWithSuggestions` and its three siblings), and `internal/api/tree.go` /
`tree_test.go` wholesale (see `web/templates/components/filters.templ`'s own "GONE" doc comment,
and [[next-session-webui-detail-list-impl]] in memory). The list is one flat, top-level-only view
now; `?status=` is a plain query param the filter form only ever *preserves* as a hidden field
(`web/templates/components/filters_test.go`'s `TestTaskFilters_StatusHiddenFieldOnlyWhenSet`) —
nothing renders a tab-specific literal for it anymore, so the "two independently hand-typed
literals must agree" hazard this seam warned about is gone along with the mechanism it warned
about, not merely undocumented.

What is left of the two ends: `WebHandler.TaskList` (`internal/api/web.go`) still passes
`q.Get("status")` straight through to `orchestrator.TaskFilter.Status`, and `store.go`'s
`ListTasks` still has no dedicated `parked` branch — the status string still falls through the
same generic fallback (`else if filter.Status != "" { conditions = append(conditions, "t.status = ?");
args = append(args, filter.Status) }`). That half is unchanged and still real,
but it is no longer a *seam* (nothing hand-types a second copy of the literal to drift out of
sync with it) — it is just a plain pass-through parameter. `TestListTasks_Parked_
ReturnsOnlyParkedTasks` (`internal/orchestrator/pre_execution_filter_store_test.go`) still pins
that `t.status = 'parked'` returns only parked tasks, which remains a fine regression guard for
the generic fallback itself, just not for a cross-file literal-agreement seam anymore.
- **When you touch it**: if a future feature reintroduces a UI element that hand-types a status
  literal to steer `ListTasks` or a row-builder (e.g. a new preset filter chip), re-derive this
  seam's shape from `orchestrator.TaskStatusParked`/`store.go`'s fallback rather than resurrecting
  the text above verbatim — the specific End A this entry described (`statusTab`) no longer
  exists to reference.

## 29. Trigger.On due predicate ↔ stuck-detection (trackSkipStreak) invariant

Any `on:` kind that can additionally veto an otherwise-every-elapsed trigger's `due` (today: only
`on: signals`, but the shape generalizes to a future third kind) can silently defeat
`TriggerLoop.trackSkipStreak`'s stuck-notification safety net if the veto is allowed to run
*before* the single-flight busy check decides what goes into `TriggerSweepResult.Skipped`.

- **End A (`internal/api/trigger_loop.go`'s `SweepTriggers`)**: computes `everyDue` (the plain
  `now - latestRun.StartedAt >= every` check, `triggerIsDue`) and checks `inFlight[key]` against
  **`everyDue` alone** — a busy, every-elapsed trigger is ALWAYS appended to `result.Skipped`,
  regardless of any `on:`-kind-specific predicate (`signalsPendingForTrigger` for `on: signals`).
  Only once a trigger is confirmed NOT busy does the final `due` (which the `on:`-specific
  predicate can still veto) decide whether `fireTrigger` runs.
- **End B (`TriggerLoop.trackSkipStreak`, same file)**: its own doc comment states the invariant
  this seam protects — a key that is genuinely stuck (in flight past `every`) must appear in
  EITHER `Skipped` or `Fired` on every subsequent sweep tick until it resolves, or the
  stuck-overrun notification (`TriggerStuckOverrunMultiplier × every`) never fires for it.
  `trackSkipStreak` has no way to tell "this key stopped appearing in Skipped because it fired"
  apart from "this key stopped appearing in Skipped because some OTHER predicate vetoed it" — it
  just deletes any key missing from this tick's `Skipped` list, silently resetting its streak.
- **Invariant**: whether a `(project, trigger)` counts as "busy" for `Skipped` purposes must be
  decided from the every-elapsed check ALONE, never from a `due` value that a later `on:`-kind
  predicate has already narrowed. A trigger that is both every-elapsed AND in-flight is *always* a
  skip, independent of what any additional activation condition says about that same instant.
- **Past break (F1, Opus review 2026-08-26, `docs/plans/signal-ingest-detailed-design.md` PR-6)**:
  the original `on: signals` implementation ANDed the signals predicate into `due` BEFORE the busy
  check (`if due && trig.On == "signals" { due = signalsPendingForTrigger(...) }; if !due {
  continue }; if busy { ... Skipped ... }`). A wedged `on: signals` job (Running forever) whose
  inbox happened to go empty — e.g. because that very job acked everything right before hanging —
  computed `due == false` from the signals predicate alone, so execution never reached the busy
  check at all: the trigger vanished from both `Skipped` and `Fired`, permanently, with no stuck
  notification ever firing (unlike an identically-configured `on: schedule` trigger, which would
  notify after `TriggerStuckOverrunMultiplier × every`).
- **Guard**: `TestSweepTriggers_OnSignals_WedgedInFlight_StaysInSkipped`
  (`internal/api/trigger_loop_test.go`) fires an `on: signals` trigger, empties its workspace's
  inbox while leaving the job Running (never completed), and asserts it is recorded in `Skipped`
  on multiple subsequent every-elapsed ticks.
- **When you touch it**: if a future `on:` kind (or any other new veto on `due`) is added, apply
  its predicate strictly AFTER the `inFlight[key]` busy check, and derive the busy check from the
  plain every-elapsed value — never from `due` post-veto. Re-run the guard test above (or add an
  analogous one for the new kind) before merging.

## 30. connector job's reduced-permission bundle — BuiltinPolicies is not the only broker gate

A "connector job gets restricted permissions" claim spans **multiple independent broker-side
gates**. Restricting one (`BuiltinPolicies`) and believing the claim covers all of them is the
break — `internal/sandbox/broker.go`'s `Handle` has TWO separate dispatch paths with no shared
enforcement point between them.

- **End A (`orchestrator.ConnectorBuiltinPolicies`, `internal/orchestrator/policy.go`, consumed by
  `dispatcher.BuildSessionJobSpec` via `SessionJobInput.ConnectorPolicy`)**: restricts which `boid`
  builtin **ops** a connector job's token may invoke (`signal_ingest`/`signal_cursor_get` only) and
  omits the `fetch` builtin entirely. This is `entry.BuiltinPolicies` in the broker.
- **End B (`SessionJobInput.HostCommands` → `orchestrator.JobSpec.HostCommands` →
  `ResolveHostCommands` → `CommandBroker.RegisterCommands`'s `commands` argument →
  `entry.Commands`)**: a COMPLETELY SEPARATE map the broker consults for any command name that is
  NOT the typed `boid`/`fetch` builtins — `broker.go`'s `handle()` calls
  `lookupCommand(entry.Commands, req.Command)` for everything else, with **no reference to
  `entry.BuiltinPolicies` at all**. A metaproject's project.yaml `host_commands:` (e.g. a `gh`
  entry backed by a real host credential/path) flows into this map for EVERY job dispatched for
  that project, connector or not, unless something explicitly empties it for the connector case.
- **Invariant**: "connector job can only reach signal_ingest/signal_cursor_get" must hold across
  BOTH gates — narrowing `BuiltinPolicies` alone leaves `entry.Commands` exactly as populated as an
  ordinary hook/exec job's, i.e. unrestricted access to every declared `host_commands:` entry.
- **Past break (code-review agent finding, PR-5,
  `docs/plans/signal-ingest-detailed-design.md` §5.2)**: the initial connector-trigger
  implementation added `SessionJobInput.ConnectorPolicy` and had `BuildSessionJobSpec` swap in
  `ConnectorBuiltinPolicies` for `BuiltinPolicies`, but left `hostCommands :=
  orchestrator.HostCommands(input.HostCommands).ToCommandDefs()` unconditional — `wire.go`'s
  `sessionDispatcherAdapter.StartExec` built `SessionJobInput.HostCommands` from the metaproject's
  `meta.HostCommands` BEFORE branching on `req.Connector != nil`, and that branch never touched it.
  A connector job could therefore invoke any host_commands entry the metaproject declared, directly
  contradicting the PR's own "only signal_ingest/signal_cursor_get" claim (Lens 2 territory: the
  claim was broader than what the diff proved).
- **Guard**: `TestBuildSessionJobSpec_ConnectorPolicyTrue_ForcesHostCommandsEmpty` /
  `_ConnectorPolicyFalse_KeepsHostCommands` (`internal/dispatcher/session_job_test.go`) pin the
  JobSpec-construction half; `TestDispatch_ConnectorPolicy_EndToEnd_HostCommandsNeverReachBroker`
  (`internal/dispatcher/runner_signal_token_test.go`) drives the full
  `BuildExecJobSpec` → `Runner.Dispatch` → `CommandBroker.RegisterCommands` chain and asserts the
  broker receives an EMPTY commands map despite a declared `host_commands.gh` entry in the input.
- **When you touch it**: any future "job type X gets reduced permissions" feature must enumerate
  and pin EVERY broker-reachable authorization surface (today: `entry.BuiltinPolicies`,
  `entry.Commands`, and the API gateway service allowlist — seam-adjacent but a different registry,
  see `Runner.registerAPIGatewayToken`/`intersectServiceNames`), not just the one the feature's own
  code path happens to touch first. A single `ConnectorPolicy`-shaped bool that forces every gate's
  restricted value at ONE place (`BuildSessionJobSpec`, not the caller) is the pattern this PR
  converged on after the fix — prefer extending that one function's `if input.ConnectorPolicy`
  block over adding a second call site that has to remember to replicate it.

## 31. blocking brokered op ↔ per-connection cancellation (`isBlockingBoidRequest`)

- **Files**: `internal/sandbox/broker.go` (`handleConn`, `isBlockingBoidRequest`, `watchConnClose`),
  plus every op that blocks server-side (today: `BoidOpTaskAsk`, `BoidOpTaskWait`).
- **What connects to what**: `handleConn` runs a request on `b.baseContext()` — the broker's
  daemon-lifetime context — and only wraps it in a connection-scoped `context.WithCancel` +
  `go watchConnClose(conn, cancel)` when `isBlockingBoidRequest(&req)` returns true. That predicate
  is an **explicit per-op allowlist**, not a property derived from the op's behavior.
- **Invariant**: every op whose executor blocks must be listed in `isBlockingBoidRequest`.
- **Why it is a seam and not a detail**: an op left off the list still works, still blocks, and
  passes every op-specific test — it just never learns that its caller died. The daemon-side wait
  keeps running (holding a goroutine, a connection FD, and whatever it polls) until daemon
  shutdown. There is **no symptom at the call site**: the sandbox is already gone, and nothing
  else severs it — there is no server-side write/idle deadline anywhere on the broker path and no
  client-side timeout in `brokerclient`.
- **Past break (self-review, PR #1032)**: `task_wait` shipped its first draft with doc comments in
  three files (`internal/api/task_wait.go`, `internal/sandbox/protocol.go`,
  `internal/server/boid_executor.go`) plus the agent-facing
  `internal/skills/data/boid-task/references/builtins.md` all stating that the broker cancels the
  wait "on daemon shutdown / sandbox disconnect" — while `isBlockingAskRequest` (as it was then
  named) was hard-coded to `req.Boid.Op == BoidOpTaskAsk`. The comments were copied from `task_ask`
  along with the executor shape; the mechanism was not. A workspace killing its own
  `boid task wait` (a `timeout` in the `run:` string, `boid job kill`, an OOM'd job container)
  would have leaked a poller per abandoned round.
- **Guard**: `TestBroker_TaskWait_ConnCloseCancelsContext`
  (`internal/sandbox/broker_task_wait_test.go`) and its twin
  `TestBroker_TaskAsk_ConnCloseCancelsContext` (`internal/sandbox/broker_test.go`) drive the live
  `handleConn` path over a real socket, close the connection mid-call, and assert the executor's
  context is cancelled. Both use `ctxBlockingExecutor`. Verified by mutation: removing
  `BoidOpTaskWait` from the switch fails the first test in ~2s.
- **When you touch it**: adding a blocking op means adding it to `isBlockingBoidRequest` AND
  writing its conn-close twin test — the `boid-add-builtin` checklist's three registries do not
  cover this one. Note also `internal/sandbox/broker_streaming_linux.go`'s `handleStreamingExec`
  calls `b.Handle(req)` with `baseContext()` unconditionally; a blocking boid op arriving with
  `Streaming=true` would bypass this seam entirely.

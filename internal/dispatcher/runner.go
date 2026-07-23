package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
	"github.com/novshi-tech/boid/internal/skills"
)

// ProjectLookup lets dispatcher resolve ProjectID → WorkspaceID and enumerate
// workspace peers, so workspace-peer authorization and peer-visibility
// concerns stay inside dispatcher instead of leaking into JobSpec.
type ProjectLookup interface {
	GetProject(id string) (*orchestrator.Project, error)
	ListProjects() ([]*orchestrator.Project, error)
}

// WorkspaceLookup reads a WorkspaceMeta for a given slug. Satisfied by
// *orchestrator.WorkspaceStore; kept as an interface so tests can stub it
// without touching disk. Load is expected to return os.ErrNotExist-wrapped
// errors when the workspace file is missing — Runner treats that as the
// "degraded window" and falls back to the global floor.
type WorkspaceLookup interface {
	Load(slug string) (*orchestrator.WorkspaceMeta, error)
}

// ProxyAllocator returns the loopback port of an HTTP(S) egress proxy bound
// to the given workspace, after applying allowed as its allowlist. The
// listener is long-lived: subsequent calls for the same workspace reuse the
// port and live-swap the allowlist. Satisfied by *sandbox.ProxyManager.
type ProxyAllocator interface {
	GetOrCreate(workspaceID string, allowed []string) (int, error)
}

// JobEventSink lets the runner report job lifecycle events to a subscriber
// (typically the web SSE hub) without taking a hard dependency on it.
// All methods are best-effort: implementations should not block or fail
// the caller — they exist to push UI refresh hints.
type JobEventSink interface {
	JobCreated(taskID, jobID string)
}

// dockerProxyState holds the lifecycle handles for a per-sandbox docker proxy.
type dockerProxyState struct {
	proxy      *dockerproxy.Server
	listener   net.Listener
	upstream   string
	socketPath string
	ledger     *dockerproxy.Ledger
}

type Runner struct {
	DB          *sql.DB
	Runtime     JobRuntime
	Broker      CommandBroker
	Sandbox     SandboxPreparer
	SecretStore *SecretStore
	Projects    ProjectLookup
	// Backend, when non-nil, overrides sandboxBackend()'s default
	// construction of a userns backend from Sandbox/Runtime/BoidBinary — a
	// test/DI seam so callers (and tests) can exercise the
	// launch/attach/resize/signal call sites against a fake
	// backend.SandboxBackend without needing a real SandboxPreparer/
	// JobRuntime pair. nil (the production default) preserves the PR1
	// behavior of always constructing the userns backend.
	Backend backend.SandboxBackend
	// Hydrator optionally resolves a project's workspace-hydrated
	// ProjectMeta (project.yaml `meta.name` plus workspace merge) by project
	// ID. It is used only for workspace-peer name resolution in
	// buildPeerAdvertise — the self project's name is already resolved at
	// JobSpec-build time via Visibility.ProjectName and does not need this.
	// nil (test wiring, or a daemon build that doesn't wire it) makes
	// buildPeerAdvertise degrade to the pre-existing basename fallback, same
	// as orchestrator.DispatchPlanner.Hydrator's nil behavior.
	Hydrator orchestrator.MetaHydrator
	// Workspaces resolves WorkspaceMeta at dispatch time for the workspace
	// the dispatched project is linked to. When nil (test wiring, missing
	// disk) the runner falls back to the global floor for proxy allowlist
	// resolution. Together with ProxyAllocator it implements the
	// workspace-scoped proxy egress allowlist (project-workspace-allowed-domains).
	Workspaces     WorkspaceLookup
	ProxyAllocator ProxyAllocator
	BoidBinary     string
	ServerSocket   string
	// ProxyPort is the default-workspace proxy port (back-compat fallback
	// when the per-workspace allocator path isn't wired or returns an
	// error). Workspaces with no overrides reuse this port via the
	// allocator's GetOrCreate("default", ...) entry.
	ProxyPort *int
	// AllowedDomains is the daemon-wide proxy egress allowlist (the floor
	// from config.yaml sandbox.allowed_domains + boid defaults). Workspace
	// overrides are added on top via orchestrator.ResolveAllowedDomains.
	AllowedDomains []string
	RuntimesDir    string
	JobEvents      JobEventSink // optional; nil disables job lifecycle broadcasts

	// GitGateway is the git gateway's job-token registry
	// (docs/plans/git-gateway-cutover.md PR4: gateway lifecycle + dispatch
	// wiring). nil disables gateway token registration entirely — Dispatch
	// and UnregisterJob treat that as a no-op rather than panicking (test
	// wiring, or a daemon build without the gateway constructed). PR4 is
	// inert: registration happens, but nothing inside the sandbox talks to
	// the gateway yet (that's PR5/PR6).
	GitGateway *gitgateway.Registry
	// GatewayURL points at the daemon's own gateway listener address string,
	// filled in by Server.Start once the gateway's TCP listener is bound —
	// the same late-binding-via-pointer pattern as ProxyPort, since the
	// gateway (like the default proxy listener) is only known once Start
	// has run. nil disables gateway URL propagation into SandboxRuntimeInfo.
	GatewayURL *string

	tokenMu       sync.Mutex
	jobTokens     map[string]string
	waiterMu      sync.Mutex
	jobWaiters    map[string]chan JobCompletionResult
	completedJobs map[string]JobCompletionResult
	runtimeMu     sync.Mutex
	taskRuntimes  map[string]map[string]struct{}
	dockerMu      sync.Mutex
	dockerStates  map[string]*dockerProxyState // keyed by runtimeID
	gatewayMu     sync.Mutex
	gatewayTokens map[string]string // jobID -> git gateway job token
	jobContextMu  sync.Mutex
	jobContexts   map[string]JobContextSnapshot // jobID -> Phase 5b PR1 task-context RPC data
}

// Dispatch launches a sandbox for the given JobSpec. The optional cleanup
// callback (typically provided by orchestrator's PlanHook for
// staging dir teardown) runs after the sandbox process has exited.
func (r *Runner) Dispatch(ctx context.Context, spec *orchestrator.JobSpec, cleanup orchestrator.CleanupFunc) (jobID string, dispatchErr error) {
	if spec == nil {
		return "", fmt.Errorf("job spec is required")
	}
	if spec.ProjectID == "" {
		return "", fmt.Errorf("job spec is missing project id")
	}
	// Argv is irrelevant when a HarnessAdapter takes over the agent process
	// (the runner-inner-child invokes adapter.Run() and ignores Argv); only
	// plain hook / exec jobs need a command to execute.
	if spec.HarnessType == "" && len(spec.Argv) == 0 {
		return "", fmt.Errorf("job spec is missing argv")
	}

	j := &Job{
		TaskID:      spec.TaskID,
		ProjectID:   spec.ProjectID,
		HandlerID:   spec.HandlerID,
		DisplayName: spec.DisplayName,
		// Role は DB ラベル / TUI 表示のみに使われる。sandbox 構築側は
		// 一切これを読まない。
		Role:           string(spec.Kind),
		ExecutionState: spec.ExecutionState,
	}
	j.ID = uuid.New().String()

	// Dispatch エラー経路の token leak 対策 (docs/plans/git-gateway-cutover.md
	// PR5 スコープ・PR4 レビュー判断メモ): the broker token (r.trackToken)
	// and the git gateway job token (r.registerGatewayToken) are both
	// registered part-way through this function, well before the sandbox
	// actually launches. Every prior version of this function relied on
	// launchSandbox succeeding (which schedules UnregisterJob via
	// cleanupSandboxAfterWait / watchRuntime) to ever revoke them — a
	// failure in ResolveHostCommands, BuildSandboxSpec, PrepareSandbox or
	// Runtime.Start after that point leaked both tokens for the rest of the
	// daemon's lifetime. UnregisterJob is a no-op for a jobID that was never
	// registered, so unconditionally calling it here on any error path is
	// the symmetric fix: one deferred call covers every return site,
	// present and future, instead of requiring each new early-return to
	// remember its own cleanup.
	defer func() {
		if dispatchErr != nil {
			r.UnregisterJob(j.ID)
		}
	}()

	if err := CreateJob(r.DB, j); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("create job: %w", err)
	}

	// Notify the web SSE hub (via the optional JobEvents sink) so task detail
	// timelines refresh as soon as a running job row exists, not only after
	// it completes. Without this the UI sits idle during the whole hook run.
	if r.JobEvents != nil && j.TaskID != "" {
		r.JobEvents.JobCreated(j.TaskID, j.ID)
	}

	// err here means Projects.GetProject itself failed (a torn registry / DB
	// read failure), not merely "no matching project row" — resolveProjectRuntime
	// returns (nil error, empty workspaceID/projectWorkDir) for the latter,
	// which is existing, deliberately-unchanged behavior (see its doc
	// comment). Silently discarding a real GetProject error here would let
	// workspaceID come back "" even though the resolution attempt failed;
	// normalizeWorkspaceSlug then maps "" to the default workspace slug, so
	// resolveWorkspaceHome below would run the *wrong* workspace's init.sh
	// (and, once PR2 wires WorkspaceHomeDir into a mount, mount the wrong
	// workspace's home) instead of failing the dispatch outright (codex
	// review, PR #787).
	workspaceID, projectWorkDir, err := r.resolveProjectRuntime(spec.ProjectID)
	if err != nil {
		err = fmt.Errorf("resolve project runtime: look up project %q: %w", spec.ProjectID, err)
		r.failJob(j, err)
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	// Workspace home ensure + init (docs/plans/home-workspace-volume.md
	// Phase 4 PR1): guarantees ~/.local/share/boid/homes/<slug> exists and,
	// if the workspace declares an init.sh, has been run for its current
	// content, before any sandbox is built for this dispatch. PR1 is
	// wiring-only — the resolved dir is threaded into rtInfo below but
	// BuildSandboxSpec does not read it yet — while init failure still fails
	// the dispatch outright (the contract's 「init 失敗時は dispatch を明示
	// エラーで fail」), matching every other pre-BuildSandboxSpec error path
	// in this function.
	workspaceHomeDir, err := r.resolveWorkspaceHome(workspaceID)
	if err != nil {
		r.failJob(j, err)
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	// Embedded skills sync (docs/plans/home-workspace-volume.md Phase 4
	// PR3): re-syncs the embedded skill set into the just-resolved workspace
	// home's ~/.claude/skills/ on every dispatch, so /boid-task /
	// /boid-orchestrate / /boid-web resolve inside claude even though the
	// claude/codex/opencode adapters no longer bind-mount them from
	// ~/.local/share/boid/skills (see internal/adapters/*/bindings.go,
	// retired this same PR). skills.DeployAll only rewrites files whose
	// content differs from the embedded copy, so this is a cheap no-op on
	// every dispatch after the first for a given boid build. A sync failure
	// fails the dispatch outright, matching every other pre-BuildSandboxSpec
	// error path in this function (including the init.sh failure just
	// above) — a job started against a stale or missing skill set would
	// otherwise silently misbehave instead of erroring loudly.
	workspaceSkillsDir := filepath.Join(workspaceHomeDir, ".claude", "skills")
	if err := skills.DeployAll(workspaceSkillsDir); err != nil {
		err = fmt.Errorf("sync embedded skills to workspace home %q: %w", workspaceSkillsDir, err)
		r.failJob(j, err)
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	workspacePeers := r.resolveWorkspacePeers(workspaceID, spec.ProjectID)

	var resolvedHostCommandsByName map[string]orchestrator.CommandDef
	if len(spec.HostCommands) > 0 || len(spec.BuiltinPolicies) > 0 {
		var err error
		_, resolvedHostCommandsByName, err = ResolveHostCommands(
			sortedKeys(spec.BuiltinPolicies),
			spec.HostCommands,
			projectWorkDir,
			exec.LookPath,
			GitOriginURL,
		)
		if err != nil {
			r.failJob(j, err)
			if cleanup != nil {
				cleanup()
			}
			return "", err
		}
	}

	var brokerSocket, brokerToken string
	// ProfileInit sandboxes scan the host filesystem for tool detection; they
	// do not call back into boid host-commands, so broker registration and the
	// broker socket mount are both skipped.
	if r.Broker != nil && sandbox.Profile(spec.SandboxProfile) != sandbox.ProfileInit &&
		(len(spec.BuiltinPolicies) > 0 || len(resolvedHostCommandsByName) > 0) {
		tokenCtx := sandbox.TokenContext{
			JobID:             j.ID,
			TaskID:            spec.TaskID,
			ProjectID:         spec.ProjectID,
			WorkspaceID:       workspaceID,
			AllowedProjectIDs: allowedProjectIDs(spec.ProjectID, workspacePeers),
			Role:              j.Role,
			ProjectDir:        projectWorkDir,
		}
		// SandboxRoot (docs/plans/git-gateway-cutover.md PR6 cutover): clone-mode
		// jobs have no host ProjectDir the sandbox's own filesystem corresponds
		// to — their cwd is always the name-scoped subdirectory of the
		// sandbox-internal sandboxCloneTargetDir ("/workspace/<name>", see
		// sandboxCloneDir / cloneDirNameForVisibility — workspace 親化リファ
		// クタリング, nose 2026-07-13 decision). See broker.entryRoot.
		if spec.Visibility.Clone != nil {
			tokenCtx.SandboxRoot = sandboxCloneDir(cloneDirNameForVisibility(spec.Visibility))
		}
		var resolve SecretResolver
		if r.SecretStore != nil {
			ns := spec.SecretNamespace
			if ns == "" {
				ns = "default"
			}
			resolve = func(key string) (string, error) {
				return r.SecretStore.Get(ns, key)
			}
		}
		// Registered under short-name keys (the "policy 用" view — see
		// ResolveHostCommands): as of 5a-3 (docs/plans/phase5-shim-and-task-
		// context.md, "5a: shim 固定ディレクトリ化" PR3 cutover) the shim's
		// bind-mount basename == its declared short name by construction
		// (sandboxShimBinDir + hostCommandSymlinks), so the shim's ExecRequest.
		// Command hits this map by direct key on every call. The broker's
		// pre-5a-3 Path-scan fallback was dropped in the same change; there
		// is no other lookup path.
		brokerToken = r.Broker.RegisterCommands(
			resolvedHostCommandsByName,
			PoliciesToSandbox(spec.BuiltinPolicies),
			tokenCtx,
			resolve,
		)
		brokerSocket = r.Broker.SocketPath()
		r.trackToken(j.ID, brokerToken)
	}

	// Validate host_commands when docker proxy is enabled: full docker access
	// via host_commands bypasses the proxy and is therefore forbidden.
	if spec.Visibility.DockerEnabled {
		if err := validateDockerHostCommands(spec.HostCommands); err != nil {
			r.failJob(j, err)
			if cleanup != nil {
				cleanup()
			}
			return "", err
		}
	}

	// Workspace-scoped proxy resolution. Both the resolved allowlist and
	// the resolved port may differ per-workspace; see resolveWorkspaceProxy
	// for the cascade (floor → workspace overrides) and the fallback rules
	// when any step fails.
	allowedDomains, proxyPort := r.resolveWorkspaceProxy(workspaceID)
	gatewayURL, gatewayToken := r.registerGatewayToken(j.ID, spec, workspaceID)

	// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): track this
	// job's routed instruction + reduced environment view + trait-filtered
	// payload so the `boid task instructions` / `boid task env` / `boid
	// task payload` broker RPCs can serve back this exact job's data — the
	// sole source for it since the Phase 5b PR6 cutover retired the
	// parallel dispatch-time context-file materialization
	// (contextFiles/buildEnvironmentYAML in sandbox_builder.go) this used to
	// also feed.
	//
	//   - Instructions comes straight from spec.Instruction (this job's own
	//     JobSpec field, resolved once by DispatchPlanner.PlanHook's
	//     selectInstruction) — NOT re-derived from the task row. Two
	//     agent-kind hooks for different agents can be dispatched from the
	//     same task in the same evaluation round (orchestrator.Evaluator
	//     matches any agent appearing anywhere in the instruction history,
	//     not just the most recent entry); only the hook whose agent matches
	//     the *last* history entry gets a non-nil Instruction. Deriving
	//     "current instructions" from the task row here would silently hand
	//     one job's agent's instruction to the other job's RPC caller — see
	//     JobContextSnapshot's own doc comment and wiring-seams.md #13.
	//   - Env uses spec.HostCommands (short-name keyed as authored in
	//     project.yaml) — the identical key space to
	//     resolvedHostCommandsByName below, no absolute-path detour after
	//     the 5a-3 cutover.
	//
	// `boid task current` does NOT need this: it re-derives live from the
	// task row (orchestrator.SnapshotTask), which carries no job-scoped
	// routing ambiguity the way instructions does.
	r.trackJobContext(j.ID, JobContextSnapshot{
		Instructions:              routedInstructionSlice(spec.Instruction),
		Env:                       BuildWorkspaceEnvView(allowedDomains, spec.HostCommands),
		Payload:                   spec.PrimaryInput,
		PayloadPatchAllowedTraits: spec.HookTraitsProduces,
	})

	// gatewayCloneURL is only worth resolving (an extra Projects lookup)
	// when the opt-in sandbox-clone path is actually declared. As of the PR6
	// cutover, planner.go / session_job.go set Visibility.Clone for every
	// project-visible job, so this now runs on the main dispatch path.
	var gatewayCloneURL, cloneWorkspaceDir string
	var peerAdvertise map[string]PeerAdvertise
	if spec.Visibility.Clone != nil {
		// Dispatch-time upstream_url requirement (docs/plans/git-gateway-cutover.md
		// 「本計画で確定する設計 § 1」: 「欠落 project は... dispatch 時エラー」).
		// A project with no captured upstream_url would otherwise silently
		// produce an empty GatewayCloneURL and fail deep inside the sandbox
		// with an opaque "git clone ''" error; failing fast here surfaces a
		// clear, actionable message to the dispatch caller instead.
		//
		// Every branch below must either succeed or hard-error — a silent
		// skip (`if err == nil && proj != nil` optimism) is exactly the
		// PR6 Opus review #4 concern: it would let a torn Projects registry
		// (project row missing / GetProject errored) fall through to a
		// runtime "git clone ''" failure inside the sandbox. The only
		// tolerated case is r.Projects == nil, which corresponds to
		// dispatcher unit tests that don't wire a Projects lookup at all
		// (the tests exercise argv/cleanup/spec plumbing, not gateway
		// resolution) — those specs also leave Visibility.Clone nil, so in
		// production this branch always runs with r.Projects non-nil.
		if r.Projects != nil {
			proj, perr := r.Projects.GetProject(spec.ProjectID)
			switch {
			case perr != nil:
				err := fmt.Errorf("clone-mode dispatch: look up project %q: %w", spec.ProjectID, perr)
				r.failJob(j, err)
				if cleanup != nil {
					cleanup()
				}
				return "", err
			case proj == nil:
				err := fmt.Errorf("clone-mode dispatch: project %q not found (registry drift?); rerun `boid project add` or check `boid project list`", spec.ProjectID)
				r.failJob(j, err)
				if cleanup != nil {
					cleanup()
				}
				return "", err
			default:
				if err := orchestrator.RequireUpstreamURL(proj); err != nil {
					r.failJob(j, err)
					if cleanup != nil {
						cleanup()
					}
					return "", err
				}
			}
		}
		gatewayCloneURL = r.buildGatewayCloneURL(spec, gatewayURL, gatewayToken)
		peerAdvertise = r.buildPeerAdvertise(workspacePeers, gatewayURL, gatewayToken)

		// /workspace host-backed runtime dir (docs/plans/git-gateway-cutover.md
		// PR6 cutover; container-based-boid.md 2026-07-08 decision: clone
		// lands on a runtime-dir bind mount by default, not tmpfs). Keyed by
		// j.ID (already a fresh UUID unique to this dispatch) rather than
		// sharing the docker-proxy's separate runtimeID/desiredRuntimeID —
		// the two concerns don't need to share a directory, and job.ID is
		// already at hand here with no extra allocation. Rides the existing
		// runtimes/ GC (24h loop, 30 day threshold) like every other
		// runtime-dir artifact; no bespoke cleanup is added.
		if r.RuntimesDir != "" {
			cloneWorkspaceDir = filepath.Join(r.RuntimesDir, j.ID, "workspace")
			if err := os.MkdirAll(cloneWorkspaceDir, 0o755); err != nil {
				slog.Warn("git gateway: failed to create clone workspace dir; falling back to sandbox-local tmpfs",
					"job_id", j.ID, "dir", cloneWorkspaceDir, "error", err)
				cloneWorkspaceDir = ""
			}
		}
	}

	rtInfo := SandboxRuntimeInfo{
		JobID:                      j.ID,
		BoidBinary:                 r.BoidBinary,
		ServerSocket:               r.ServerSocket,
		ProxyPort:                  proxyPort,
		BrokerSocket:               brokerSocket,
		BrokerToken:                brokerToken,
		WorkspacePeers:             workspacePeers,
		WorkspacePeerAdvertise:     peerAdvertise,
		ResolvedHostCommandsByName: resolvedHostCommandsByName,
		AllowedDomains:             allowedDomains,
		GatewayURL:                 gatewayURL,
		GatewayJobToken:            gatewayToken,
		GatewayCloneURL:            gatewayCloneURL,
		CloneWorkspaceDir:          cloneWorkspaceDir,
		WorkspaceHomeDir:           workspaceHomeDir,
		WorkspaceSlug:              filepath.Base(workspaceHomeDir),
		ContainerImage:             r.resolveContainerImage(workspaceID),
	}
	// Server socket is only exposed to jobs that have no broker policies
	// attached — i.e. boid exec invocations that need to talk to the daemon
	// directly. For hook/gate jobs the daemon conversation goes through the
	// broker socket above.
	if brokerToken != "" {
		rtInfo.ServerSocket = ""
	}

	// Per-sandbox docker proxy setup: start before BuildSandboxSpec so the
	// socket path can be injected into env and mounts.
	var desiredRuntimeID string
	if spec.Visibility.DockerEnabled && r.RuntimesDir != "" {
		runtimeID := uuid.New().String()
		ds, err := r.startDockerProxy(runtimeID)
		if err != nil {
			// Non-fatal: log and continue without docker proxy rather than
			// blocking the job entirely. The sandbox will start but DOCKER_HOST
			// won't be set.
			slog.Warn("docker proxy: failed to start, docker unavailable for this job",
				"job_id", j.ID, "error", err)
		} else {
			rtInfo.ProxySocketPath = ds.socketPath
			desiredRuntimeID = runtimeID
			r.trackDockerState(runtimeID, ds)
		}
	}

	sbSpec, err := BuildSandboxSpec(spec, rtInfo)
	if err != nil {
		if desiredRuntimeID != "" {
			r.stopDockerProxy(desiredRuntimeID)
		}
		r.failJob(j, err)
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}
	return r.launchSandbox(ctx, j, sbSpec, cleanup, desiredRuntimeID)
}

// resolveProjectRuntime resolves projectID to its (WorkspaceID, WorkDir).
// A non-nil error means the GetProject call itself failed (e.g. a torn
// registry / DB read failure) and callers must treat that as fatal to the
// dispatch rather than silently continuing with an empty workspaceID
// (codex review, PR #787). A nil error with both return strings empty
// covers two deliberately-conflated "no workspace" cases that callers do
// NOT treat as fatal: r.Projects unset / projectID empty, and a
// non-nil-error GetProject that simply found no matching project row
// (proj == nil) — existing behavior, kept as-is here.
func (r *Runner) resolveProjectRuntime(projectID string) (string, string, error) {
	if r.Projects == nil || projectID == "" {
		return "", "", nil
	}
	proj, err := r.Projects.GetProject(projectID)
	if err != nil || proj == nil {
		return "", "", err
	}
	return proj.WorkspaceID, proj.WorkDir, nil
}

// resolveWorkspacePeers enumerates projects sharing workspaceID other than
// selfID, returning a peer-id → host-path map suitable for both broker
// authorization (AllowedProjectIDs) and sandbox FS mounting. Returns nil when
// workspaceID is empty, Projects is unset, or the lookup fails — callers treat
// nil as "no peers" and a solo-project allowlist.
func (r *Runner) resolveWorkspacePeers(workspaceID, selfID string) map[string]string {
	if r.Projects == nil || workspaceID == "" {
		return nil
	}
	projects, err := r.Projects.ListProjects()
	if err != nil {
		return nil
	}
	peers := make(map[string]string)
	for _, p := range projects {
		if p == nil || p.ID == "" || p.ID == selfID {
			continue
		}
		if p.WorkspaceID != workspaceID {
			continue
		}
		peers[p.ID] = p.WorkDir
	}
	if len(peers) == 0 {
		return nil
	}
	return peers
}

func (r *Runner) proxyPort() int {
	if r.ProxyPort == nil {
		return 0
	}
	return *r.ProxyPort
}

// resolveWorkspaceProxy returns the proxy egress allowlist and the loopback
// port that should be passed to the sandbox for a job running under the
// given workspace.
//
// Cascade:
//  1. Start from the daemon-wide floor (r.AllowedDomains).
//  2. If ProxyAllocator and a non-empty workspaceID are present, load the
//     workspace.yaml (best-effort: ErrNotExist is the documented degraded
//     window, other errors are warned). Add the workspace AllowedDomains
//     on top of the floor via orchestrator.ResolveAllowedDomains.
//  3. Ask ProxyAllocator.GetOrCreate to bind (or live-update) a listener
//     for the workspace with that resolved list, and return its port.
//
// Fallback: if any step fails — allocator unwired, workspaceID empty,
// allocator returns an error — the function returns the floor and the
// default-workspace port (r.proxyPort()). Dispatch is never blocked on a
// proxy-resolution problem.
func (r *Runner) resolveWorkspaceProxy(workspaceID string) ([]string, int) {
	if r.ProxyAllocator == nil || workspaceID == "" {
		return r.AllowedDomains, r.proxyPort()
	}
	var wsMeta *orchestrator.WorkspaceMeta
	if r.Workspaces != nil {
		loaded, err := r.Workspaces.Load(workspaceID)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("workspace load for proxy allowlist failed; using floor only",
					"workspace_id", workspaceID, "error", err)
			}
		} else {
			wsMeta = loaded
		}
	}
	resolved := orchestrator.ResolveAllowedDomains(r.AllowedDomains, wsMeta)
	port, err := r.ProxyAllocator.GetOrCreate(workspaceID, resolved)
	if err != nil {
		slog.Warn("workspace proxy listener allocation failed; falling back to default proxy",
			"workspace_id", workspaceID, "error", err)
		return r.AllowedDomains, r.proxyPort()
	}
	return resolved, port
}

// resolveContainerImage returns the workspace's Phase 6 container image
// override (WorkspaceMeta.ContainerImage), or "" when unset, unwired
// (r.Workspaces == nil / workspaceID == ""), or on a load failure — the same
// best-effort, dispatch-must-never-block-on-this posture as
// resolveWorkspaceProxy's own independent r.Workspaces.Load call, which this
// mirrors rather than reuses (each concern loads its own WorkspaceMeta view,
// consistent with the existing AllowedDomains precedent). "" here always
// means "use the container backend's configured default image" downstream —
// this function has no notion of what that default is, and, as of PR5, no
// caller wires containerBackend into dispatch at all (docs/plans/
// phase6-container-backend.md §PR5: config 非公開, cutover is PR7).
func (r *Runner) resolveContainerImage(workspaceID string) string {
	if r.Workspaces == nil || workspaceID == "" {
		return ""
	}
	wsMeta, err := r.Workspaces.Load(workspaceID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("workspace load for container image failed; using backend default",
				"workspace_id", workspaceID, "error", err)
		}
		return ""
	}
	if wsMeta == nil {
		return ""
	}
	return wsMeta.ContainerImage
}

// startDockerProxy creates a per-sandbox docker proxy socket and starts the
// proxy server. Returns the dockerProxyState on success; the caller must call
// stopDockerProxy on error or when the sandbox exits.
//
// The socket is placed next to the boid server socket (not inside runtimeDir)
// to stay under the 108-byte Unix domain socket path limit. Long test
// environment paths (e.g. /tmp/boid-e2e-<scenario>-xxx/data/boid/runtimes/UUID)
// would exceed the limit if the socket were placed there.
func (r *Runner) startDockerProxy(runtimeID string) (*dockerProxyState, error) {
	upstream, err := dockerproxy.ResolveUpstream("")
	if err != nil {
		return nil, fmt.Errorf("resolve docker upstream: %w", err)
	}
	runtimeDir := filepath.Join(r.RuntimesDir, runtimeID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir docker proxy runtime dir: %w", err)
	}
	// Place the socket in the boid server socket directory (short path) rather
	// than inside runtimeDir. Long E2E scenario names make runtimeDir exceed
	// the 108-byte Unix socket path limit (EINVAL on bind).
	socketPath := r.dockerProxySocketPath(runtimeID)
	ledgerPath := filepath.Join(runtimeDir, "docker-resources.jsonl")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen docker proxy socket: %w", err)
	}
	// Restrict access to the owner only: sandbox processes run as the same
	// uid and need access; other users must not reach the proxy.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod docker proxy socket: %w", err)
	}

	ledger := dockerproxy.NewLedger(ledgerPath)
	proxy := dockerproxy.NewWithLedger(upstream, ledger)
	go func() {
		if err := proxy.Serve(ln); err != nil {
			slog.Debug("docker proxy serve ended", "runtime_id", runtimeID, "error", err)
		}
	}()
	slog.Info("docker proxy started", "runtime_id", runtimeID, "socket", socketPath)
	return &dockerProxyState{
		proxy:      proxy,
		listener:   ln,
		upstream:   upstream,
		socketPath: socketPath,
		ledger:     ledger,
	}, nil
}

// dockerProxySocketPath returns a short socket path for the per-sandbox docker
// proxy. Unix domain sockets on Linux have a 108-byte path limit (EINVAL on
// bind). Long test or system paths can push the proxy socket over this limit,
// so the socket is placed next to the boid server socket rather than inside
// the deep runtimeDir hierarchy.
//
// Falls back to the runtimeDir path when ServerSocket is not configured (e.g.
// in unit tests that construct a minimal Runner).
func (r *Runner) dockerProxySocketPath(runtimeID string) string {
	const maxUnixSocketPath = 107
	if r.ServerSocket != "" {
		// Short name uses first 12 hex chars of the UUID to avoid collisions
		// across concurrent jobs while staying well under the path limit.
		short := filepath.Join(filepath.Dir(r.ServerSocket), runtimeID[:12]+".dp.s")
		if len(short) <= maxUnixSocketPath {
			return short
		}
	}
	// Fallback (ServerSocket unset or still too long): use runtimeDir.
	return filepath.Join(r.RuntimesDir, runtimeID, "docker-proxy.sock")
}

func (r *Runner) trackDockerState(runtimeID string, ds *dockerProxyState) {
	r.dockerMu.Lock()
	defer r.dockerMu.Unlock()
	if r.dockerStates == nil {
		r.dockerStates = make(map[string]*dockerProxyState)
	}
	r.dockerStates[runtimeID] = ds
}

func (r *Runner) rekeyDockerState(oldID, newID string) {
	r.dockerMu.Lock()
	defer r.dockerMu.Unlock()
	if r.dockerStates == nil {
		return
	}
	if ds, ok := r.dockerStates[oldID]; ok {
		delete(r.dockerStates, oldID)
		r.dockerStates[newID] = ds
	}
}

// stopDockerProxy closes the proxy and removes its state from the map.
func (r *Runner) stopDockerProxy(runtimeID string) {
	r.dockerMu.Lock()
	ds, ok := r.dockerStates[runtimeID]
	if ok {
		delete(r.dockerStates, runtimeID)
	}
	r.dockerMu.Unlock()
	if !ok {
		return
	}
	if err := ds.proxy.Close(); err != nil {
		slog.Debug("docker proxy close", "runtime_id", runtimeID, "error", err)
	}
}

// reapAndCloseDockerProxy calls Reap to clean up docker resources created by
// the sandbox job, then closes the proxy server. Called after the sandbox exits
// (success or failure) so cleanup always runs.
func (r *Runner) reapAndCloseDockerProxy(runtimeID string) {
	r.dockerMu.Lock()
	ds, ok := r.dockerStates[runtimeID]
	if ok {
		delete(r.dockerStates, runtimeID)
	}
	r.dockerMu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dockerproxy.Reap(ctx, ds.upstream, ds.ledger); err != nil {
		slog.Warn("docker reap failed", "runtime_id", runtimeID, "error", err)
	}
	if err := ds.proxy.Close(); err != nil {
		slog.Debug("docker proxy close", "runtime_id", runtimeID, "error", err)
	}
}

// validateDockerHostCommands returns an error when the spec registers a
// docker host command without subcommand restrictions, which would allow
// sandbox processes to bypass the docker proxy by calling docker directly.
func validateDockerHostCommands(hostCommands map[string]orchestrator.CommandDef) error {
	cmd, ok := hostCommands["docker"]
	if !ok {
		return nil
	}
	// If AllowedSubcommands is non-empty or AllowedPatterns is non-empty, the
	// registration is subcommand-restricted (e.g. build-only) and is acceptable.
	if len(cmd.AllowedSubcommands) > 0 || len(cmd.AllowedPatterns) > 0 {
		return nil
	}
	return fmt.Errorf("host_commands.docker: unrestricted docker access bypasses the docker proxy " +
		"(capabilities.docker is enabled); remove docker from host_commands or restrict to specific " +
		"subcommands (e.g. allow: [build])")
}

func allowedProjectIDs(selfID string, workspacePeers map[string]string) []string {
	seen := make(map[string]struct{})
	var ids []string
	if selfID != "" {
		seen[selfID] = struct{}{}
		ids = append(ids, selfID)
	}
	if len(workspacePeers) == 0 {
		return ids
	}
	var peers []string
	for id := range workspacePeers {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		peers = append(peers, id)
	}
	sort.Strings(peers)
	return append(ids, peers...)
}

func (r *Runner) trackToken(jobID, token string) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if r.jobTokens == nil {
		r.jobTokens = make(map[string]string)
	}
	r.jobTokens[jobID] = token
}

// failJob marks j as failed in the DB. Used for errors that occur after
// CreateJob but before the sandbox is launched, so orphan running rows do not
// accumulate in the jobs table.
func (r *Runner) failJob(j *Job, cause error) {
	j.Status = JobStatusFailed
	j.Output = cause.Error()
	if err := UpdateJob(r.DB, j); err != nil {
		slog.Warn("persist pre-launch job failure", "job_id", j.ID, "error", err)
	}
}

// WaitForJob registers a channel that will receive the job completion result.
func (r *Runner) WaitForJob(jobID string) <-chan JobCompletionResult {
	r.waiterMu.Lock()
	defer r.waiterMu.Unlock()

	ch := make(chan JobCompletionResult, 1)
	if result, ok := r.completedJobs[jobID]; ok {
		ch <- result
		return ch
	}
	if r.jobWaiters == nil {
		r.jobWaiters = make(map[string]chan JobCompletionResult)
	}
	r.jobWaiters[jobID] = ch
	return ch
}

// CompleteJob signals the waiting dispatcher that a job has completed.
func (r *Runner) CompleteJob(jobID string, result JobCompletionResult) {
	r.waiterMu.Lock()
	if r.completedJobs == nil {
		r.completedJobs = make(map[string]JobCompletionResult)
	}
	r.completedJobs[jobID] = result
	ch, ok := r.jobWaiters[jobID]
	if ok {
		delete(r.jobWaiters, jobID)
	}
	r.waiterMu.Unlock()
	if ok {
		ch <- result
	}
}

// sandboxBackend returns the SandboxBackend Runner launches through. PR1
// (docs/plans/phase6-container-backend.md) wires only the userns backend,
// built fresh from Sandbox/Runtime/BoidBinary on every call — it owns no
// state, so there is nothing to cache. A config-driven choice between
// userns/container backends (sandbox.backend: userns|container) lands in
// PR5+; until then this is the sole selection point. r.Backend, when set,
// overrides this (test/DI seam — see its doc comment).
func (r *Runner) sandboxBackend() backend.SandboxBackend {
	if r.Backend != nil {
		return r.Backend
	}
	return newUsernsBackend(r.Sandbox, r.Runtime, r.BoidBinary)
}

// launchSandbox launches a sandbox for job via the configured
// SandboxBackend and persists the resulting runtime metadata.
func (r *Runner) launchSandbox(ctx context.Context, job *Job, spec sandbox.Spec, cleanup orchestrator.CleanupFunc, desiredRuntimeID string) (string, error) {
	if job == nil {
		return "", fmt.Errorf("job is required")
	}
	if r.Sandbox == nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("sandbox preparer is required")
	}
	if r.Runtime == nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("job runtime is required")
	}

	session, err := r.sandboxBackend().Launch(ctx, spec, backend.LaunchOptions{
		JobID:     job.ID,
		TaskID:    job.TaskID,
		ProjectID: job.ProjectID,
		HandlerID: job.HandlerID,
		Role:      job.Role,

		Interactive: spec.TTY,
		TTY:         spec.TTY,
		DesiredID:   desiredRuntimeID,
		// StdinForward: only `boid exec` (job.Role == JobKindExec) ever needs a
		// live stdin forwarder on the non-interactive (pipe) transport — a hook
		// job reading stdin must keep seeing an immediate EOF (see
		// RuntimeStartSpec.StdinForward's doc comment). No-op when TTY/Interactive
		// is true (the PTY branch ignores this field).
		StdinForward: job.Role == string(orchestrator.JobKindExec),
	})
	if err != nil {
		// Only a JobRuntime.Start-phase failure tears down the
		// desiredRuntimeID docker proxy pre-allocated before Start ran — a
		// PrepareSandbox-phase failure never did (pre-Phase-6 behavior,
		// preserved via usernsStartError; see its doc comment).
		var startErr *usernsStartError
		if desiredRuntimeID != "" && errors.As(err, &startErr) {
			r.stopDockerProxy(desiredRuntimeID)
		}
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	// session is handled purely through the backend.SandboxSession
	// interface from here on — no concrete-type assertion — so a future
	// container backend (PR5, docs/plans/phase6-container-backend.md) that
	// returns a different SandboxSession implementation needs no change
	// here. Interactive/TTY are read back from the already-known spec.TTY
	// (identical to what LaunchOptions.Interactive/TTY carried into Launch)
	// rather than from the session, since the userns backend's contract is
	// to echo them back unchanged (LocalRuntime.Start does exactly that) —
	// see sessionLocalArtifacts for the one piece of userns-specific state
	// (PrepareSandbox's on-disk artifacts) that genuinely has no
	// backend-agnostic equivalent yet, handled via a capability probe
	// instead.
	handleID := session.ID()

	// When DesiredID was set, the proxy was pre-registered under desiredRuntimeID.
	// After Start, handleID may differ only if the runtime didn't honour DesiredID
	// (e.g. a test stub). In that case, re-key the dockerState entry.
	if desiredRuntimeID != "" && handleID != desiredRuntimeID {
		r.rekeyDockerState(desiredRuntimeID, handleID)
	}

	job.RuntimeID = handleID
	job.Interactive = spec.TTY
	job.TTY = spec.TTY
	if err := UpdateJob(r.DB, job); err != nil {
		_ = session.Stop(context.Background())
		r.stopDockerProxy(handleID)
		cleanupSandboxArtifacts(sessionLocalArtifacts(session))
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("persist job runtime metadata: %w", err)
	}

	r.trackTaskRuntime(job.TaskID, handleID)
	go r.watchRuntime(job.ID, session)
	go r.cleanupSandboxAfterWait(session, cleanup)
	slog.Info("job started", "job_id", job.ID, "runtime_id", handleID)
	return job.ID, nil
}

func (r *Runner) cleanupSandboxAfterWait(session backend.SandboxSession, extra orchestrator.CleanupFunc) {
	defer func() {
		if extra != nil {
			extra()
		}
	}()
	if session == nil {
		return
	}
	runtimeID := session.ID()
	prepared := sessionLocalArtifacts(session)
	if runtimeID == "" || prepared == nil {
		return
	}
	result, err := session.Wait(context.Background())
	if err != nil {
		if errors.Is(err, ErrRuntimeUnsupported) {
			// Reap docker resources even on unsupported-wait paths (best effort).
			r.reapAndCloseDockerProxy(runtimeID)
			cleanupSandboxArtifacts(prepared)
			return
		}
		slog.Warn("skip sandbox cleanup: runtime wait failed", "runtime_id", runtimeID, "error", err)
		return
	}

	// Docker Reap + proxy Close: run unconditionally (success or failure) and
	// before the runtime dir is removed so the ledger is still readable.
	r.reapAndCloseDockerProxy(runtimeID)

	// Scaffolding (RootDir, StagingDir) は runner-outer が常に削除するので、
	// ここでは保険として idempotent に rm するだけ。 exit_code に関わらず実行。
	cleanupSandboxScaffolding(prepared)
	// The spec file carries secrets (broker token / API keys), so it is removed
	// unconditionally — even on failure. The redacted runner-state.json is the
	// diagnostic artifact retained for post-hoc analysis instead.
	cleanupSandboxSpec(prepared)
	if result.ExitCode != 0 {
		// silent な exit_code != 0 ケースの事後解析を可能にするため、 runner-state
		// だけ保全する。 transcript.log が 0 byte で daemon log にも有用情報が無い
		// 場合、 runner-state.json (spec dump + 到達 phase) がほぼ唯一の手がかりに
		// なる。 GC や手動削除に任せる。
		slog.Warn("retained runner-state for diagnosis (exit_code!=0)",
			"runtime_id", runtimeID,
			"exit_code", result.ExitCode,
			"state_path", prepared.StatePath,
		)
		return
	}
	cleanupSandboxState(prepared)
}

// cleanupSandboxArtifacts removes every sandbox artifact (scaffolding + spec +
// state). Used by runtime-unsupported paths and tests.
func cleanupSandboxArtifacts(prepared *PreparedSandbox) {
	cleanupSandboxScaffolding(prepared)
	cleanupSandboxSpec(prepared)
	cleanupSandboxState(prepared)
}

// cleanupSandboxScaffolding removes the sandbox ROOT directory and the staging
// dir. Both are normally rm'd by runner-outer; this call is a best-effort safety
// net for the case where runner-outer was killed before its cleanup ran.
func cleanupSandboxScaffolding(prepared *PreparedSandbox) {
	if prepared == nil {
		return
	}
	if prepared.RootDir != "" {
		if err := os.RemoveAll(prepared.RootDir); err != nil {
			slog.Warn("remove sandbox root", "path", prepared.RootDir, "error", err)
		}
	}
	if prepared.StagingDir != "" {
		if err := os.RemoveAll(prepared.StagingDir); err != nil {
			slog.Warn("remove sandbox staging dir", "path", prepared.StagingDir, "error", err)
		}
	}
}

// cleanupSandboxSpec removes the JSON sandbox spec file (carries secrets, so it
// is removed unconditionally).
func cleanupSandboxSpec(prepared *PreparedSandbox) {
	if prepared == nil || prepared.SpecPath == "" {
		return
	}
	if err := os.Remove(prepared.SpecPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("remove sandbox spec", "path", prepared.SpecPath, "error", err)
	}
}

// cleanupSandboxState removes the runner-state.json diagnostic file. It is
// deliberately retained on exit_code != 0 for post-hoc diagnosis.
func cleanupSandboxState(prepared *PreparedSandbox) {
	if prepared == nil || prepared.StatePath == "" {
		return
	}
	if err := os.Remove(prepared.StatePath); err != nil && !os.IsNotExist(err) {
		slog.Warn("remove runner-state", "path", prepared.StatePath, "error", err)
	}
}

// StopJobRuntime stops the runtime identified by runtimeID.
// It is a best-effort operation: errors are logged at debug level only.
func (r *Runner) StopJobRuntime(runtimeID string) {
	if r.Runtime == nil || runtimeID == "" {
		return
	}
	if err := r.Runtime.Stop(context.Background(), runtimeID); err != nil {
		slog.Debug("stop job runtime", "runtime_id", runtimeID, "error", err)
	}
}

// SignalJobRuntime delivers a single signal to the runtime's process group
// without any SIGKILL follow-up. NotifyTask uses this for SIGUSR1 to ask the
// agent (run-agent.py) to stop the agent session gracefully — the go-native
// runner subcommands keep the signal SIG_IGN (inherited across execve), so they
// survive while run-agent.py acts on it and runner-inner-child still posts
// `boid job done` through the broker. Best-effort: errors at debug level only.
//
// Routes through SandboxBackend.Adopt → SandboxSession.Signal
// (docs/plans/phase6-container-backend.md §PR1: this is the `boid agent
// stop` seam the plan calls out by name) rather than reaching into
// r.Runtime directly, so a future container backend's signal-forwarding
// entrypoint (§決定 3) is reachable from the same call site.
func (r *Runner) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	if r.Runtime == nil || runtimeID == "" {
		return
	}
	session, ok := r.sandboxBackend().Adopt(context.Background(), runtimeID)
	if !ok {
		return
	}
	if err := session.Signal(context.Background(), sig); err != nil {
		slog.Debug("signal job runtime", "runtime_id", runtimeID, "signal", sig, "error", err)
	}
}

// ReapOrphans reconciles sandbox resources a previous daemon instance left
// behind, via the configured SandboxBackend (docs/plans/
// phase6-container-backend.md §PR7 / §決定 6). It is a thin delegation —
// r.sandboxBackend().ReapOrphans(ctx) — so callers (internal/server/wire.go's
// startup sequence) never need to know which backend is live: the userns
// backend's ReapOrphans is a permanent no-op stub, so calling this on every
// daemon startup is always safe and only does real work when the container
// backend is selected (config sandbox.backend: container).
//
// Callers MUST run this — and act on ReapReport.FailedJobIDs, e.g. by
// skipping auto-reopen for the corresponding tasks — strictly BEFORE
// resuming any daemon_shutdown-aborted task, per §決定 6: a docker
// container does not die when the daemon process restarts the way a
// userns child process (killed by MarkStaleJobsFailed's implicit process
// death) does, so auto-reopening before reap could dispatch a fresh agent
// against a task whose previous job container is still alive — two agents
// mutating the same $HOME/task RPC concurrently.
func (r *Runner) ReapOrphans(ctx context.Context) (backend.ReapReport, error) {
	return r.sandboxBackend().ReapOrphans(ctx)
}

// CanAttach reports whether runtimeID can currently be adopted by the
// configured SandboxBackend — i.e. whether an attach/resize/signal ingress
// against it should be allowed. This is the single source of truth for
// "is this runtime attachable" now that a live session may be held by any
// SandboxBackend implementation, not just JobRuntime's own in-memory
// session map: internal/server/job_runtime_routes.go's resolveAttachableJob
// used to answer this by type-asserting runtime.jobRuntime onto a
// runtimeAttachSupport (SupportsAttach) interface directly, bypassing the
// SandboxBackend/SandboxSession seam entirely — for the userns backend that
// happened to still give the right answer (since the JobRuntime it wraps is
// the same one), but a future container backend's session may not be
// backed by a JobRuntime session map at all, so that check would (wrongly)
// always fail for it. Routing through Adopt fixes both: userns behavior is
// unchanged (usernsBackend.Adopt itself now probes JobRuntime's
// SupportsAttach capability, see its doc comment), and any future backend
// answers with its own notion of session liveness (codex review Blocker 2
// on PR #816).
func (r *Runner) CanAttach(ctx context.Context, runtimeID string) bool {
	if runtimeID == "" {
		return false
	}
	_, ok := r.sandboxBackend().Adopt(ctx, runtimeID)
	return ok
}

// ResizeRuntimeID resizes a sandbox session's terminal via the configured
// SandboxBackend, given a runtimeID directly. This is the collapse point
// for the HTTP resize ingress (POST /api/jobs/{id}/resize,
// internal/server/job_runtime_routes.go — the caller there has already
// resolved job.RuntimeID and doesn't go through the jobID-keyed
// RuntimeInputWriter path the WS transport uses; see ResizeRuntime for
// that one). docs/plans/phase6-container-backend.md §PR1 lists this as one
// of the two resize ingress routes that must route through
// SandboxSession.Resize.
func (r *Runner) ResizeRuntimeID(ctx context.Context, runtimeID string, size TerminalSize) error {
	if runtimeID == "" {
		return fmt.Errorf("runtime id is required")
	}
	session, ok := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !ok {
		return ErrRuntimeUnsupported
	}
	return session.Resize(size)
}

// CleanupTaskWindow stops all tracked runtimes associated with a task.
func (r *Runner) CleanupTaskWindow(taskID string) {
	if r.Runtime == nil {
		return
	}
	runtimeIDs := r.takeTaskRuntimes(taskID)
	for _, runtimeID := range runtimeIDs {
		if err := r.Runtime.Stop(context.Background(), runtimeID); err != nil {
			slog.Debug("cleanup task runtime", "task_id", taskID, "runtime_id", runtimeID, "error", err)
		}
	}
}

// WaitForJobCtx waits for job completion with context cancellation.
//
// A non-zero exit is NOT reported as an error — the caller inspects
// result.ExitCode. Only true wait-machinery failures (ctx cancel) produce a
// non-nil error. This lets the orchestrator record `hook_fired` actions for
// failing hooks the same way as successful ones; prior behavior discarded
// the partial FiredEvents when any hook exited non-zero.
func (r *Runner) WaitForJobCtx(ctx context.Context, jobID string) (JobCompletionResult, error) {
	ch := r.WaitForJob(jobID)
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		return JobCompletionResult{}, fmt.Errorf("wait for job %s: %w", jobID, ctx.Err())
	}
}

// UnregisterJob removes the broker token and the git gateway job token
// associated with the given job.
func (r *Runner) UnregisterJob(jobID string) {
	r.tokenMu.Lock()
	token, ok := r.jobTokens[jobID]
	if ok {
		delete(r.jobTokens, jobID)
	}
	r.tokenMu.Unlock()

	if ok && r.Broker != nil {
		r.Broker.UnregisterCommandToken(token)
		slog.Info("unregistered broker token", "job_id", jobID)
	}

	r.gatewayMu.Lock()
	gwToken, gwOK := r.gatewayTokens[jobID]
	if gwOK {
		delete(r.gatewayTokens, jobID)
	}
	r.gatewayMu.Unlock()

	if gwOK && r.GitGateway != nil {
		r.GitGateway.Unregister(gwToken)
		slog.Info("unregistered git gateway token", "job_id", jobID)
	}

	r.untrackJobContext(jobID)
}

func (r *Runner) isJobCompleted(jobID string) bool {
	r.waiterMu.Lock()
	defer r.waiterMu.Unlock()
	_, ok := r.completedJobs[jobID]
	return ok
}

func (r *Runner) trackTaskRuntime(taskID, runtimeID string) {
	if taskID == "" || runtimeID == "" {
		return
	}
	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()
	if r.taskRuntimes == nil {
		r.taskRuntimes = make(map[string]map[string]struct{})
	}
	if r.taskRuntimes[taskID] == nil {
		r.taskRuntimes[taskID] = make(map[string]struct{})
	}
	r.taskRuntimes[taskID][runtimeID] = struct{}{}
}

func (r *Runner) takeTaskRuntimes(taskID string) []string {
	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()

	runtimes := r.taskRuntimes[taskID]
	if len(runtimes) == 0 {
		return nil
	}
	delete(r.taskRuntimes, taskID)

	out := make([]string, 0, len(runtimes))
	for runtimeID := range runtimes {
		out = append(out, runtimeID)
	}
	sort.Strings(out)
	return out
}

func (r *Runner) watchRuntime(jobID string, session backend.SandboxSession) {
	if session == nil {
		return
	}
	runtimeID := session.ID()
	if runtimeID == "" {
		return
	}
	result, err := session.Wait(context.Background())
	if err != nil {
		if errors.Is(err, ErrRuntimeUnsupported) {
			return
		}
		slog.Warn("runtime wait failed", "job_id", jobID, "runtime_id", runtimeID, "error", err)
		return
	}
	if r.isJobCompleted(jobID) {
		return
	}
	job, err := GetJob(r.DB, jobID)
	if err != nil {
		slog.Warn("runtime exited for unknown job", "job_id", jobID, "runtime_id", runtimeID, "error", err)
		return
	}
	if job.Status != JobStatusRunning {
		return
	}

	exitCode := result.ExitCode
	if exitCode == 0 {
		exitCode = 1
	}
	output := fmt.Sprintf("job runtime exited without boid job done (runtime_id=%s, exit_code=%d)", runtimeID, result.ExitCode)

	job.Status = JobStatusFailed
	job.ExitCode = exitCode
	job.Output = output
	if err := UpdateJob(r.DB, job); err != nil {
		slog.Warn("persist runtime exit failure state", "job_id", jobID, "runtime_id", runtimeID, "error", err)
		return
	}

	r.CompleteJob(jobID, JobCompletionResult{
		Output:   output,
		ExitCode: exitCode,
	})
	r.UnregisterJob(jobID)

	// transcript size を一緒に出すと、 0 byte なら「子プロセスが PTY に何も書け
	// ずに死んだ silent failure」と即時に判別できる。 transcript path は
	// retainSandboxArtifacts と合わせて事後解析の起点になる。
	transcriptSize, transcriptErr := transcriptSizeBytes(result.TranscriptPath)
	slog.Warn("runtime exited before boid job done",
		"job_id", jobID,
		"runtime_id", runtimeID,
		"exit_code", result.ExitCode,
		"transcript_path", result.TranscriptPath,
		"transcript_size", transcriptSize,
		"transcript_stat_error", transcriptErr,
	)
}

// transcriptSizeBytes は transcript.log のサイズを返す。 path が空 / stat 失敗
// の場合は (-1, error message) を返す。 watchRuntime の log で silent failure
// を判別するために使う。
func transcriptSizeBytes(path string) (int64, string) {
	if path == "" {
		return -1, "no transcript path"
	}
	info, err := os.Stat(path)
	if err != nil {
		return -1, err.Error()
	}
	return info.Size(), ""
}

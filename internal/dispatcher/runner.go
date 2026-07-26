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

// NoWorkspaceProxyKey is the ProxyAllocator key the daemon uses, once at
// startup (internal/server's Server.Start), to bind a dedicated egress
// proxy listener for jobs with no workspace at all (`boid exec` /
// ProfileInit against an unlinked project — see resolveWorkspaceProxy's own
// doc comment). Exported so internal/server can seed this listener under
// the exact same key resolveWorkspaceProxy looks up via
// Runner.NoWorkspaceProxyPort, without either package hard-coding the
// other's literal.
//
// Deliberately DISTINCT from orchestrator.DefaultWorkspaceSlug (BLOCKER,
// codex review round 3): the real "default"-slug workspace has its own
// workspace.yaml an operator can add AllowedDomains to, and
// ProxyManager.GetOrCreate mutates a listener's allowlist in place — keying
// the no-workspace listener by the SAME string as that real workspace would
// let a later default-workspace dispatch silently widen the no-workspace
// listener too. Contains "_", a character orchestrator.ValidWorkspaceSlug
// never permits in a real workspace slug (only lowercase letters, digits,
// and "-"), so this key can never collide with a real workspace's own
// allocator key, now or in the future.
const NoWorkspaceProxyKey = "__no_workspace__"

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
	Broker      CommandBroker
	SecretStore *SecretStore
	Projects    ProjectLookup
	// Backend is the SandboxBackend every sandbox launch/attach/resize/
	// signal call site routes through (sandboxBackend() below). Production
	// wiring (internal/server/wire.go's buildRuntime) always sets this to a
	// real containerBackend — container is the only sandbox backend since
	// PR-4 removed the userns backend (docs/plans/volume-only-daemon.md
	// §論点e) and the config-driven userns/container branch that used to
	// leave this nil for the userns default. Tests set it to a fake
	// backend.SandboxBackend directly (the same DI seam this field has
	// always been).
	Backend backend.SandboxBackend
	// InstallID mirrors ContainerBackendOptions.InstallID (§決定6): threaded
	// through separately here (not read back off Backend) so
	// startDockerProxy's per-job SetWorkspaceNetwork call (PR9, §決定5) can
	// compute the exact same containerWorkspaceNetworkName the container
	// backend's own Launch computed for this job's workspace, without a
	// type assertion back into *containerBackend. Empty (every pre-PR9
	// caller) is valid — containerWorkspaceNetworkName treats it as
	// "noinst", matching NewContainerBackend's own unset-InstallID default.
	InstallID string
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
	// NoWorkspaceProxyPort is the port of the dedicated egress proxy
	// listener internal/server's Server.Start binds once at daemon startup,
	// keyed by NoWorkspaceProxyKey, for jobs with no workspace at all
	// (workspaceID == "" — see resolveWorkspaceProxy's doc comment). Same
	// late-binding-via-pointer pattern as ProxyPort (Start fills in the int
	// this points at after the listener is actually bound). nil (test
	// wiring, or a daemon build that never calls Start) makes
	// resolveWorkspaceProxy's empty-workspaceID branch return port 0 —
	// deliberately NEVER ProxyPort's value: falling back to the real
	// default workspace's own (editable, live-widening) listener is exactly
	// the aliasing bug NoWorkspaceProxyKey's distinctness exists to
	// prevent, and a port-0 dispatch failing loudly is safer than a silent
	// isolation leak.
	NoWorkspaceProxyPort *int
	// AllowedDomains is the daemon-wide proxy egress allowlist floor
	// (built-in defaults ∪ config.yaml's sandbox.allowed_domains), captured
	// once when this Runner is built (internal/server/wire.go's
	// buildRuntime). Workspace overrides are added on top per-dispatch via
	// orchestrator.ResolveAllowedDomains.
	//
	// A plain []string, not a getter — sandbox.allowed_domains is
	// ReloadRestartRequired (PR #830 round-4 simplification, nose
	// directive; see ReloadDynamic's own doc comment in
	// internal/config/schema.go): `boid config set sandbox.allowed_domains
	// ...` persists to config.yaml immediately but only reaches this field
	// on the next daemon restart, when wire.go rebuilds the Runner from the
	// freshly-loaded config. nil is a valid, cheap-to-construct test
	// default meaning "no floor".
	AllowedDomains []string
	RuntimesDir    string
	// DataHomeDir is the daemon's OWN persistent data root —
	// server/wire.go's dataHomeFor(cfg), which in a real deployment is
	// filepath.Dir(cfg.DBPath): the directory boid.db / web_secret /
	// install_id / secret.key / tls/ / kits/ already live in, which
	// under the volume-only compose deploy is the `boid_state` named volume.
	// (dataHomeFor also has a socket-path fallback and an empty case that
	// only test wiring reaches — see its own doc comment.)
	//
	// Deliberately NOT derivable from RuntimesDir: since the volume-only
	// pivot those are two genuinely different roots (RuntimesDir resolves via
	// hostVisibleRuntimesDirFor(cfg) to BOID_RUNTIME_DIR, host-visible but
	// tmpfs). Currently consumed only by workspaceHomeMetaDir — the workspace
	// home init marker, the init lock, and the private temp copy of init.sh
	// (docs/plans/workspace-home-volume-persistence.md 論点b, PR2).
	//
	// Empty (minimal test wiring, or a daemon build that never wired it)
	// falls back to $XDG_DATA_HOME/boid; see workspaceHomeMetaDir's own doc
	// comment. That fallback resolves against the ambient environment, so any
	// package whose tests construct a bare &Runner{} and reach Dispatch must
	// install testutil/homeenv's TestMain isolation or it will operate on the
	// developer's real boid installation.
	DataHomeDir string
	JobEvents   JobEventSink // optional; nil disables job lifecycle broadcasts

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
	// GatewayCAPEM points at the daemon's internal CA's own PEM-encoded
	// certificate (mtls.CA.CertPEM), filled in by Server.Start alongside
	// GatewayURL. A container-backend clone-mode job needs this to trust
	// the git gateway's TLS server certificate (see
	// SandboxRuntimeInfo.GatewayCAPEM's own doc comment for why a client
	// certificate is not also needed). nil disables CA propagation.
	GatewayCAPEM *[]byte

	// GatewayCredentials resolves forge auth for the daemon's OWN direct
	// git operations against a bare repo (docs/plans/volume-only-daemon.md
	// §論点b, PR-2b) — the same gitgateway.CredentialProvider bare_repo.go's
	// CloneBareRepo/FetchBareRepo already consume (see those functions'
	// own doc comments for why this is a direct CredentialProvider.Resolve
	// call, not a round trip through the HTTP reverse proxy: there is no
	// job token or sandbox involved for this specific use — Dispatch calls
	// FetchBareRepo itself, from the daemon process, before staging a
	// per-job checkout via PrepareJobCheckout). nil (every pre-PR-2b
	// caller, and any test wiring that doesn't need per-job clone) skips
	// the FetchBareRepo refresh step entirely — matches CloneBareRepo/
	// FetchBareRepo's own "creds nil -> proceed unauthenticated" fail-open
	// convention, appropriate here too since a project's bare repo may
	// simply be public.
	GatewayCredentials *gitgateway.CredentialProvider

	// WithProjectLock, when set, runs a function while holding the daemon's
	// project lifecycle mutex — api.ProjectAppService.projectMu, via that
	// service's own exported WithProjectLock method — the same lock
	// CreateProject/CreateProjectFromGitURL/DeleteProject/FetchProject
	// already serialize their own create/delete/fetch critical sections
	// against each other (PR834 PR-2b round-2 codex review Major 1). The
	// entire project-registry-guarded dispatch section below — gateway-token
	// registration, the selfProject lookup, the gatewayCloneURL/
	// peerAdvertise snapshot, managed-bare-repo classification, AND the
	// FetchBareRepo + PrepareJobCheckout pair against selfProject.WorkDir —
	// runs as one closure inside this lock (round-3 codex review Major 1:
	// round-2 only wrapped the final fetch+clone call, leaving the lookup
	// and URL/token snapshot that feed it unguarded — see Dispatch's own
	// "project-registry-guarded dispatch section" comment for the mixed-
	// checkout race that left open), so a concurrent `project rm` + re-add
	// at the identical managed path cannot land ANYWHERE in that sequence
	// and produce a mixed checkout (this project's gateway URL/credentials
	// cloned against a DIFFERENT, just-re-registered project's bare-repo
	// content).
	//
	// internal/dispatcher cannot import internal/api directly to call
	// ProjectAppService.WithProjectLock itself — internal/api already
	// imports internal/dispatcher for wiring, so the reverse direction would
	// be an import cycle — so internal/server/wire.go closes over the same
	// ProjectAppService instance's WithProjectLock method and hands it here
	// as a plain function value instead (see wire.go's assignment, right
	// next to GatewayCredentials above, which is wired from the identical
	// gwCreds for the identical reason: no cross-package type dependency,
	// just a shared instance closed over once at startup).
	//
	// nil (any dispatcher unit test that does not wire an
	// api.ProjectAppService at all, and any pre-PR-2b caller) means "no
	// serialization" — the per-job-clone step below runs unguarded exactly
	// as it did before this field existed. Production wiring always sets it.
	WithProjectLock func(fn func() error) error

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
	checkoutMu    sync.Mutex
	checkoutDirs  map[string]string // jobID -> per-job clone staging dir (docs/plans/volume-only-daemon.md §論点b, PR-2b)
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
	//
	// Both return values are used: workspaceSlug is the normalized slug this
	// call just resolved workspaceID into, threaded to rtInfo.WorkspaceSlug
	// below. It is deliberately NOT re-derived from workspaceHomeDir — see
	// resolveWorkspaceHome's doc comment (PR4 of
	// docs/plans/workspace-home-volume-persistence.md) for why that derivation
	// breaks the moment PR6 turns the home into a named volume.
	workspaceHomeDir, workspaceSlug, err := r.resolveWorkspaceHome(workspaceID)
	if err != nil {
		r.failJob(j, err)
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	// Embedded skills (docs/plans/workspace-home-volume-persistence.md 論点
	// e-2, PR3): materializes the embedded skill set under the host-visible
	// runtimes root and prepares the per-skill bind targets inside the
	// workspace home, so /boid-task / /boid-orchestrate / /boid-web resolve
	// inside the harness even though the claude/codex/opencode adapters no
	// longer declare bind mounts for them (internal/adapters/*/bindings.go).
	// The returned directory is threaded into rtInfo below, where homeMounts
	// turns it into one read-only bind per skill.
	//
	// Phase 4 PR3 (docs/plans/home-workspace-volume.md) instead copy-synced
	// the content into <workspaceHomeDir>/.claude/skills — a direct daemon
	// write into the workspace HOME, which PR6 turns into a named volume the
	// daemon cannot write to. See syncEmbeddedSkills for why the destination
	// moved rather than the timing, and why the call stays per-dispatch.
	//
	// A failure fails the dispatch outright, matching every other
	// pre-BuildSandboxSpec error path in this function (including the init.sh
	// failure just above) — a job started against a stale or missing skill
	// set would otherwise silently misbehave instead of erroring loudly.
	skillsSourceDir, err := r.syncEmbeddedSkills(workspaceHomeDir)
	if err != nil {
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
	// gatewayCAPEM: see SandboxRuntimeInfo.GatewayCAPEM's own doc comment.
	// r.GatewayCAPEM nil (every pre-PR9-fix caller/test, or a daemon with
	// no TLS CA configured) leaves this nil, matching gatewayURL's own
	// "unwired" degrade. Daemon-level CA material, not per-project, so this
	// stays outside the project-registry-guarded section below.
	var gatewayCAPEM []byte
	if r.GatewayCAPEM != nil {
		gatewayCAPEM = *r.GatewayCAPEM
	}

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
	// routing ambiguity the way instructions does. No project-registry
	// dependency either, so this also runs outside the locked section below.
	r.trackJobContext(j.ID, JobContextSnapshot{
		Instructions:              routedInstructionSlice(spec.Instruction),
		Env:                       BuildWorkspaceEnvView(allowedDomains, spec.HostCommands),
		Payload:                   spec.PrimaryInput,
		PayloadPatchAllowedTraits: spec.HookTraitsProduces,
	})

	// --- project-registry-guarded dispatch section (PR834 PR-2b round-3
	// codex review Major 1) -------------------------------------------------
	// Round-2's fix only wrapped the final fetch+clone call
	// (FetchBareRepo/PrepareJobCheckout) in r.WithProjectLock. Everything
	// that FEEDS that call still ran BEFORE the lock was acquired:
	// gateway-token registration (registerGatewayToken -> buildGatewayRepos,
	// which reads THIS project's own upstream_url to build its self-repo
	// permission entry), the selfProject lookup (WorkDir/UpstreamURL), the
	// gatewayCloneURL/peerAdvertise snapshot (buildGatewayCloneURL/
	// buildPeerAdvertise each independently re-read r.Projects too), and the
	// managed-bare-repo classification
	// (orchestrator.IsBareRepoDir(selfProject.WorkDir)).
	//
	// A concurrent `project rm` + re-add at the identical (workspace, name)
	// path can land in that pre-lock window — SafeBareRepoPath is
	// deterministic on those two inputs alone, not on project id or
	// content, so "remove and re-add at the same slot" is an ordinary
	// operator flow, not just an adversarial one. selfProject.WorkDir is
	// just a path string captured before the race; by the time round-2's
	// lock finally ran the fetch+clone, that path could already hold a
	// DIFFERENT repository's content while gatewayCloneURL/gatewayToken
	// were still built from the OLD project's upstream_url — exactly the
	// "clone repository B using project A's gateway route/credentials"
	// mismatch this closes. Wrapping the entire read-then-clone sequence in
	// one r.WithProjectLock closure means it now either observes
	// spec.ProjectID's project consistently from lookup through clone, or
	// (WithProjectLock unset — dispatcher unit tests that don't wire an
	// api.ProjectAppService) runs unguarded exactly as it did before this
	// fix existed.
	var (
		gatewayURL, gatewayToken           string
		gatewayCloneURL, cloneWorkspaceDir string
		peerAdvertise                      map[string]PeerAdvertise
		cloneHostBacked                    bool
	)
	// selfProject is hoisted to this scope (mirrors the pre-fix code) purely
	// for readability; nothing outside the closure reads it.
	var selfProject *orchestrator.Project
	dispatchProjectSection := func() error {
		gatewayURL, gatewayToken = r.registerGatewayToken(j.ID, spec, workspaceID)

		// gatewayCloneURL is only worth resolving (an extra Projects lookup)
		// when the opt-in sandbox-clone path is actually declared. As of the
		// PR6 cutover, planner.go / session_job.go set Visibility.Clone for
		// every project-visible job, so this now runs on the main dispatch
		// path.
		if spec.Visibility.Clone == nil {
			return nil
		}

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
				return fmt.Errorf("clone-mode dispatch: look up project %q: %w", spec.ProjectID, perr)
			case proj == nil:
				return fmt.Errorf("clone-mode dispatch: project %q not found (registry drift?); rerun `boid project add` or check `boid project list`", spec.ProjectID)
			default:
				if err := orchestrator.RequireUpstreamURL(proj); err != nil {
					return err
				}
				selfProject = proj
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

		// Per-job clone at dispatch time (docs/plans/volume-only-daemon.md
		// §論点b "採用経路 (per-job clone、clone 時代整合)", PR-2b): a
		// git-URL-registered project (orchestrator.IsBareRepoDir(selfProject.
		// WorkDir), PR-2a) dispatched under the container backend gets its
		// /workspace/<name> pre-populated by the DAEMON, straight from its
		// own bare repo cache, instead of leaving the sandbox's own
		// in-container clone sequence (buildCloneSpec/performCloneSteps) to
		// fetch it via the git gateway's HTTP reverse proxy at container
		// start. This is the architecture decision the plan doc's own
		// §論点b spells out as the worktree-retraction replacement: a
		// per-job `git clone file://<bare-repo>` into a staging dir
		// under r.RuntimesDir (host-visible under container backend as of
		// this same PR's wire.go fix — see hostVisibleRuntimesDirFor's own
		// doc comment), bind-mounted into the job container directly.
		//
		// Every OTHER combination (userns backend, a legacy host-dir-
		// registered project, or cloneWorkspaceDir empty because
		// r.RuntimesDir itself is unset) leaves cloneHostBacked false and
		// falls through to the pre-PR-2b in-sandbox clone path unchanged —
		// this is additive, not a replacement of that path.
		if IsContainerBackend(r.Backend) && cloneWorkspaceDir != "" && selfProject != nil && orchestrator.IsBareRepoDir(selfProject.WorkDir) {
			// Step 1 (plan doc): bring the bare repo cache up to date before
			// staging off of it. Best-effort — a transient fetch failure
			// degrades to dispatching against whatever the cache already
			// has, matching §論点a's auto-prune-retirement invariant
			// ("filesystem/remote 観測を根拠に... hard delete しない"; the
			// same spirit applies here: a stale-but-present cache must not
			// block dispatch outright).
			if r.GatewayCredentials != nil {
				if ferr := FetchBareRepo(ctx, selfProject.WorkDir, r.GatewayCredentials, spec.SecretNamespace); ferr != nil {
					slog.Warn("per-job clone: refresh bare repo failed; dispatching against the existing cache",
						"job_id", j.ID, "project_id", spec.ProjectID, "error", ferr)
				}
			}
			// Steps 2-3 (plan doc): stage the per-job checkout directly into
			// cloneWorkspaceDir — no extra project-name subdirectory needed,
			// cloneMounts already binds this whole directory at
			// sandboxCloneDir(name) below. remoteURL rewrites `origin` to
			// the gateway clone URL so a writable job's own in-sandbox
			// `git push` still routes through the gateway (token auth,
			// notify-on-401) instead of a meaningless daemon-local bare
			// repo path — see PrepareJobCheckout's own doc comment.
			cd := spec.Visibility.Clone
			if err := PrepareJobCheckout(ctx, selfProject.WorkDir, cd.Branch, cd.BaseBranch, cd.BaseBranchForkPoint, gatewayCloneURL, cloneWorkspaceDir); err != nil {
				return fmt.Errorf("per-job clone: %w", err)
			}
			r.trackCheckoutDir(j.ID, cloneWorkspaceDir)
			cloneHostBacked = true
		}
		return nil
	}
	// The closure above now runs as a single r.WithProjectLock critical
	// section when wired (production — see WithProjectLock's own doc
	// comment); nil only for dispatcher unit tests that never wire an
	// api.ProjectAppService, matching every pre-existing nil-safety
	// convention in this file.
	var projectSectionErr error
	if r.WithProjectLock != nil {
		projectSectionErr = r.WithProjectLock(dispatchProjectSection)
	} else {
		projectSectionErr = dispatchProjectSection()
	}
	if projectSectionErr != nil {
		r.failJob(j, projectSectionErr)
		if cleanup != nil {
			cleanup()
		}
		return "", projectSectionErr
	}

	// [Blocker 2, PR7 codex review]: proxyHost stays "" (applyProxyEnv's own
	// hostGatewayIP fallback) for the userns backend — byte-for-byte
	// unchanged. The container backend has no 10.0.2.2 projection at all,
	// so a docker-enabled job needs the compose egress service DNS name
	// instead; see SandboxRuntimeInfo.ProxyHost's own doc comment.
	var proxyHost string
	if IsContainerBackend(r.Backend) {
		proxyHost = composeEgressServiceName
	}

	rtInfo := SandboxRuntimeInfo{
		JobID:                      j.ID,
		BoidBinary:                 r.BoidBinary,
		ServerSocket:               r.ServerSocket,
		ProxyPort:                  proxyPort,
		ProxyHost:                  proxyHost,
		UsingContainerBackend:      IsContainerBackend(r.Backend),
		BrokerSocket:               brokerSocket,
		BrokerToken:                brokerToken,
		WorkspacePeers:             workspacePeers,
		WorkspacePeerAdvertise:     peerAdvertise,
		ResolvedHostCommandsByName: resolvedHostCommandsByName,
		AllowedDomains:             allowedDomains,
		GatewayURL:                 gatewayURL,
		GatewayCAPEM:               gatewayCAPEM,
		GatewayJobToken:            gatewayToken,
		GatewayCloneURL:            gatewayCloneURL,
		CloneWorkspaceDir:          cloneWorkspaceDir,
		CloneHostBacked:            cloneHostBacked,
		WorkspaceHomeDir:           workspaceHomeDir,
		WorkspaceSlug:              workspaceSlug,
		SkillsSourceDir:            skillsSourceDir,
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

			// Workspace network forced injection (PR9, §決定5): only for the
			// container backend — the userns backend has no per-workspace
			// docker network infrastructure at all (containerBackend.Launch
			// is the only thing that ever creates one, via
			// ensureWorkspaceNetwork), so calling SetWorkspaceNetwork
			// unconditionally would force every sibling a userns-backend
			// docker-enabled job creates onto a network nothing ever
			// created, breaking every existing docker-proxy-* e2e scenario
			// (those run against the userns backend + a fake docker). The
			// network NAME must match containerBackend.Launch's own
			// ensureWorkspaceNetwork call for this same (installID,
			// workspace) pair exactly — see containerWorkspaceNetworkName's
			// own doc comment for why this is a pure function both call
			// sites compute independently rather than shared state.
			if IsContainerBackend(r.Backend) && workspaceID != "" {
				ds.proxy.SetWorkspaceNetwork(containerWorkspaceNetworkName(r.InstallID, workspaceID))
			}
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
	return r.launchSandbox(ctx, j, sbSpec, cleanup, desiredRuntimeID, workspaceID, spec.Visibility.DockerEnabled)
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

// noWorkspaceProxyPort returns the dedicated no-workspace listener's port
// (see NoWorkspaceProxyPort's own doc comment). Deliberately does NOT fall
// back to r.proxyPort() when unwired — that port belongs to the real,
// editable default-slug workspace's own listener, and falling back to it
// here would be precisely the aliasing bug NoWorkspaceProxyKey's
// distinctness exists to prevent (BLOCKER, codex review round 3; see
// resolveWorkspaceProxy's own doc comment). An unwired
// NoWorkspaceProxyPort (test wiring that doesn't set it, or a daemon build
// that never calls Server.Start) yields port 0 — dispatch fails loudly
// rather than silently widening what a no-workspace job can reach.
func (r *Runner) noWorkspaceProxyPort() int {
	if r.NoWorkspaceProxyPort == nil {
		return 0
	}
	return *r.NoWorkspaceProxyPort
}

// resolveWorkspaceProxy returns the proxy egress allowlist and the loopback
// port that should be passed to the sandbox for a job running under the
// given workspace.
//
// A job with no workspace at all (workspaceID == "" — `boid exec` or a
// ProfileInit sandbox against a project with no workspace assigned) always
// gets r.AllowedDomains (the floor, no workspace overrides — there is no
// workspace.yaml to load) and r.noWorkspaceProxyPort() — a port bound ONCE
// at daemon startup (internal/server's Server.Start), keyed by
// NoWorkspaceProxyKey, a reserved string DISTINCT from
// orchestrator.DefaultWorkspaceSlug (BLOCKER, codex review round 3: keying
// this path by DefaultWorkspaceSlug itself — the SAME key the named-
// workspace branch below uses for the real, editable "default"-slug
// workspace — let that workspace's own workspace.yaml AllowedDomains
// silently widen the SAME shared listener a no-workspace job was using).
// This never re-resolves or re-allocates per dispatch (PR #830 round-4
// simplification, nose directive: sandbox.allowed_domains is
// ReloadRestartRequired now, so there is nothing to refresh mid-process —
// see ReloadDynamic's own doc comment, internal/config/schema.go), which
// also makes this path immune to round-4 blocker 1 (a runtime allocation
// failure falling back to the widened default listener): there is no
// runtime allocation call here to fail.
//
// A job WITH a workspace (non-empty workspaceID) still goes through the
// live cascade:
//  1. Start from the daemon-wide floor (r.AllowedDomains).
//  2. If ProxyAllocator is present, load the workspace.yaml (best-effort:
//     ErrNotExist is the documented degraded window, other errors are
//     warned). Add the workspace AllowedDomains on top of the floor via
//     orchestrator.ResolveAllowedDomains.
//  3. Ask ProxyAllocator.GetOrCreate to bind (or live-update) a listener
//     for the workspace with that resolved list, and return its port.
//
// If ProxyAllocator is nil or GetOrCreate errors, this branch falls back to
// r.proxyPort() — the default-workspace listener — which is the documented,
// pre-existing (not round-2/3) fallback for a NAMED workspace only; it is
// never reachable from the empty-workspaceID branch above.
func (r *Runner) resolveWorkspaceProxy(workspaceID string) ([]string, int) {
	if workspaceID == "" {
		return r.AllowedDomains, r.noWorkspaceProxyPort()
	}
	if r.ProxyAllocator == nil {
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

// sandboxBackend returns the SandboxBackend Runner launches through —
// r.Backend directly (PR-4, docs/plans/volume-only-daemon.md §論点e): the
// userns fallback construction this function used to fall back to when
// r.Backend was nil is gone along with the userns backend itself, since
// production wiring (internal/server/wire.go) now always sets Backend to a
// real containerBackend. A nil r.Backend here is a caller/test wiring bug,
// not a valid "use the default backend" signal any more.
func (r *Runner) sandboxBackend() backend.SandboxBackend {
	return r.Backend
}

// launchSandbox launches a sandbox for job via the configured
// SandboxBackend and persists the resulting runtime metadata.
//
// workspace and dockerEnabled (PR9, §決定5) are threaded through explicitly
// from Dispatch's own already-resolved workspaceID / spec.Visibility.
// DockerEnabled (the *orchestrator.JobSpec, not the sandbox.Spec parameter
// below, which carries neither) — see backend.LaunchOptions.Workspace/
// DockerEnabled's own doc comments for why containerBackend needs both and
// why this was previously a wiring gap (both fields were left at their
// zero value on every real Dispatch call, silently disabling per-job
// docker-capability delivery and workspace network isolation for the
// container backend specifically — the userns backend never read either
// field, so nothing exercised the gap before PR9).
func (r *Runner) launchSandbox(ctx context.Context, job *Job, spec sandbox.Spec, cleanup orchestrator.CleanupFunc, desiredRuntimeID string, workspace string, dockerEnabled bool) (string, error) {
	if job == nil {
		return "", fmt.Errorf("job is required")
	}
	if r.Backend == nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("sandbox backend is required")
	}

	session, err := r.sandboxBackend().Launch(ctx, spec, backend.LaunchOptions{
		JobID:     job.ID,
		TaskID:    job.TaskID,
		ProjectID: job.ProjectID,
		HandlerID: job.HandlerID,
		Role:      job.Role,

		Workspace:     workspace,
		DockerEnabled: dockerEnabled,

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
		// Tear down the desiredRuntimeID docker proxy pre-allocated before
		// Launch ran — any Launch failure means the container was never
		// successfully started, so nothing else will ever clean it up.
		// (Pre-PR-4, this only fired on a JobRuntime.Start-phase failure
		// specifically, distinguished via the userns backend's own
		// usernsStartError sentinel — containerBackend.Launch has no
		// analogous two-phase prepare/start split to distinguish, so that
		// distinction no longer applies.)
		if desiredRuntimeID != "" {
			r.stopDockerProxy(desiredRuntimeID)
		}
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}

	// session is handled purely through the backend.SandboxSession
	// interface from here on — no concrete-type assertion. Interactive/TTY
	// are read back from the already-known spec.TTY (identical to what
	// LaunchOptions.Interactive/TTY carried into Launch) rather than from
	// the session, since containerBackend's contract is to echo them back
	// unchanged.
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

// cleanupSandboxAfterWait waits for the session to exit and reaps/closes its
// per-sandbox docker proxy. All sandbox-local artifact cleanup (spec/state
// files, scaffolding dirs) is owned by the session's own backend now —
// containerSession.waitLoop removes its own specPath/dockerTLSDir material
// by the time Wait returns below — since PR-4 removed the userns backend's
// PreparedSandbox capability probe this function used to delegate that
// cleanup through (docs/plans/volume-only-daemon.md §論点e).
//
// [Major 6, PR7 codex review, still relevant]: reap/close must run
// regardless of backend — this is what makes a docker-enabled job's sibling
// resources (created by the *client* inside the sandbox — docker CLI,
// TestContainers, ... — via the per-job dockerproxy) get reaped and the
// per-sandbox dockerproxy server itself get closed, for every job.
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
	if runtimeID == "" {
		return
	}
	_, err := session.Wait(context.Background())
	if err != nil {
		if errors.Is(err, ErrRuntimeUnsupported) {
			// Reap docker resources even on unsupported-wait paths (best effort).
			r.reapAndCloseDockerProxy(runtimeID)
			return
		}
		slog.Warn("skip sandbox cleanup: runtime wait failed", "runtime_id", runtimeID, "error", err)
		return
	}

	// Docker Reap + proxy Close: run unconditionally (success or failure) and
	// before the runtime dir is removed so the ledger is still readable.
	r.reapAndCloseDockerProxy(runtimeID)
}

// StopJobRuntime stops the runtime identified by runtimeID.
// It is a best-effort operation: errors are logged at debug level only.
//
// [Blocker 3, PR7 codex review]: routes through SandboxBackend.Adopt →
// SandboxSession.Stop (the same seam SignalJobRuntime/ResizeRuntimeID/
// CanAttach already use, docs/plans/phase6-container-backend.md §PR1) so a
// task abort/`boid task stop` reaches the actual container the job is
// running in via Adopt(runtimeID), not a backend-specific transport this
// function would otherwise have to know about directly.
func (r *Runner) StopJobRuntime(runtimeID string) {
	if runtimeID == "" {
		return
	}
	session, ok := r.sandboxBackend().Adopt(context.Background(), runtimeID)
	if !ok {
		return
	}
	if err := session.Stop(context.Background()); err != nil {
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
// stop` seam the plan calls out by name).
func (r *Runner) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	if r.Backend == nil || runtimeID == "" {
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
// startup sequence) never need to know backend construction details;
// container is the only sandbox backend since PR-4, so this always does
// real reconciliation work now.
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
// against it should be allowed. Routes through Adopt so the backend answers
// with its own notion of session liveness (codex review Blocker 2 on PR
// #816) rather than internal/server/job_runtime_routes.go's
// resolveAttachableJob type-asserting onto a backend-specific capability
// directly.
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
//
// [Blocker 3, PR7 codex review]: routes through StopJobRuntime (itself
// SandboxBackend.Adopt → SandboxSession.Stop, see its doc comment) so
// aborting a task actually stops its job container, not just an in-memory
// bookkeeping entry.
func (r *Runner) CleanupTaskWindow(taskID string) {
	runtimeIDs := r.takeTaskRuntimes(taskID)
	for _, runtimeID := range runtimeIDs {
		r.StopJobRuntime(runtimeID)
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
	r.cleanupCheckoutDir(jobID)
}

// trackCheckoutDir records jobID's per-job clone staging dir (docs/plans/
// volume-only-daemon.md §論点b, PR-2b) so cleanupCheckoutDir can remove it
// once the job completes — the same jobID-keyed tracked-resource pattern
// r.gatewayTokens/r.jobTokens already use for their own per-job cleanup.
func (r *Runner) trackCheckoutDir(jobID, stagingDir string) {
	if jobID == "" || stagingDir == "" {
		return
	}
	r.checkoutMu.Lock()
	defer r.checkoutMu.Unlock()
	if r.checkoutDirs == nil {
		r.checkoutDirs = make(map[string]string)
	}
	r.checkoutDirs[jobID] = stagingDir
}

// cleanupCheckoutDir removes jobID's per-job clone staging dir, if any
// (docs/plans/volume-only-daemon.md §論点b step 5: "job 終了時、 staging
// area を削除"). Called from UnregisterJob so this runs on every job-
// completion path that already calls it (CompleteJob's normal exit,
// watchRuntime's "exited without boid job done" path, and Dispatch's own
// early-failure paths — see UnregisterJob's other call sites). A missing
// entry (jobID never reached the per-job-clone step, e.g. a legacy
// host-dir-registered project or the userns backend) is a silent no-op —
// the common case for every dispatch this PR does not touch.
func (r *Runner) cleanupCheckoutDir(jobID string) {
	r.checkoutMu.Lock()
	stagingDir, ok := r.checkoutDirs[jobID]
	if ok {
		delete(r.checkoutDirs, jobID)
	}
	r.checkoutMu.Unlock()

	if !ok {
		return
	}
	if err := CleanupJobCheckout(stagingDir); err != nil {
		slog.Warn("per-job clone: cleanup staging dir failed", "job_id", jobID, "staging_dir", stagingDir, "error", err)
	}
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

package dispatcher

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/registry"
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// SandboxRuntimeInfo carries the dispatcher-internal facts that are required
// to turn an orchestrator.JobSpec into a sandbox.Spec but that orchestrator
// never needs to know: job id, broker plumbing, proxy port, boid binary
// location, server socket path, staging dirs.
type SandboxRuntimeInfo struct {
	JobID        string
	BoidBinary   string
	ServerSocket string
	ProxyPort    int
	// ProxyHost is the host applyProxyEnv points HTTP_PROXY/HTTPS_PROXY at.
	// Empty falls back to hostGatewayIP ("10.0.2.2", the pasta/slirp
	// userns gateway IP) — applyProxyEnv's own doc comment. Runner.Dispatch
	// sets this to the compose egress service DNS name
	// (dispatcher-internal composeEgressServiceName, "boid-egress") when
	// the configured backend is the container backend
	// (IsContainerBackend(r.Backend)): a docker sibling container has no
	// 10.0.2.2 projection at all, so a container-backend job needs a
	// reachable compose-network address instead.
	ProxyHost string

	// WorkspaceNetworkCIDRs are the subnets of this job's OWN per-workspace
	// docker network, which applyProxyEnv adds to no_proxy/NO_PROXY so a
	// sibling container dialed by container IP is reached directly instead of
	// through the egress proxy. Runner.Dispatch fills this in via the
	// backend's optional workspaceNetworkCIDRResolver; nil (every non-container
	// backend, and a container backend whose subnet lookup failed) leaves
	// no_proxy exactly as it was before this field existed.
	//
	// Only this workspace's own subnets belong here — see
	// containerBackend.WorkspaceNetworkCIDRs' doc comment for why widening it
	// would hand the job a route into other workspaces' networks.
	WorkspaceNetworkCIDRs []string

	// UsingContainerBackend is Runner.Dispatch's IsContainerBackend(r.Backend)
	// — the same signal ProxyHost's own doc comment describes for the
	// identical "computed once at the Dispatch call site, threaded through
	// because BuildSandboxSpec has no other way to know the configured
	// backend" reason. BuildSandboxSpec uses it to skip binding rt.BoidBinary
	// into the sandbox (see its own call site below): the container backend's
	// shared image already bakes boid at the fixed shim path
	// (build/container/Dockerfile), so binding it AGAIN would try to
	// bind-mount rt.BoidBinary (os.Executable() — resolves to the DAEMON's
	// own in-image path, e.g. "/usr/local/bin/boid") as a
	// docker-out-of-docker sibling mount SOURCE, which the host's real
	// docker daemon then rejects outright ("bind source path does not
	// exist") since that path only exists inside the daemon's own
	// container, never on the real host filesystem a DooD sibling's bind
	// sources are resolved against.
	UsingContainerBackend bool

	BrokerSocket string
	BrokerToken  string

	// WorkspacePeers maps peer project IDs (same workspace, excluding self) to
	// host paths. Dispatcher resolves this from its ProjectLookup so peer
	// visibility/authorization does not leak into orchestrator.JobSpec.
	WorkspacePeers map[string]string

	// Foreground indicates whether the job runs in the foreground (user-facing
	// stdout/stderr, no trap-based completion callback). boid exec sets this
	// to true; hook/gate jobs leave it false so stdout is captured and a
	// `boid job done` trap posts completion back to the daemon.
	Foreground bool

	// ResolvedHostCommandsByName is the short-name-keyed view of the resolved
	// host command defs produced by ResolveHostCommands. It is the single
	// source of truth for host command wiring:
	//
	//   - the broker's policy table (CommandBroker.RegisterCommands) keys off
	//     it directly;
	//   - buildHostCommandRulesEnv turns the same map into
	//     BOID_HOST_COMMAND_RULES;
	//   - hostCommandSymlinks (5a-3) materializes one
	//     `/run/boid/bin/<name> -> boid` symlink per entry — so every
	//     host_command becomes a shim on PATH under its declared short name,
	//     even when host_commands.<name>.path aliases the source file to a
	//     different basename.
	//
	// An earlier absolute-path-keyed sibling (ResolvedHostCommands / byPath)
	// was dropped: it existed only to key the retired hostCommandMounts /
	// buildHostCommandNamesEnv / per-command PATH parent, all replaced by the
	// fixed-directory scheme. Empty when the job declares no host commands.
	ResolvedHostCommandsByName map[string]orchestrator.CommandDef

	// ProxySocketPath, when non-empty, is the host-side Unix socket path of the
	// per-sandbox docker proxy. sandbox_builder bind-mounts it into the sandbox
	// at the fixed sandbox path (see dockerProxySandboxSocket) and injects
	// DOCKER_HOST / CONTAINER_HOST / TESTCONTAINERS_* env vars.
	// Set by the runner before BuildSandboxSpec when capabilities.docker is
	// declared in project.yaml.
	ProxySocketPath string

	// AllowedDomains is the proxy egress allowlist. It is purely informational
	// inside the sandbox (the proxy itself enforces it on the host), surfaced
	// to the agent via the `boid task env` broker RPC so it
	// knows which hosts are reachable without burning a turn on a 403.
	AllowedDomains []string

	// GatewayURL is the git gateway's sandbox-facing base URL
	// (http://10.0.2.2:<port>), set by Runner from the daemon's own
	// gateway listener. Empty when the gateway isn't wired.
	GatewayURL string

	// GatewayCAPEM is the daemon's internal CA's own certificate
	// (mtls.CA.CertPEM), PEM-encoded, set by Runner from Server's
	// gatewayCAPEM. Non-secret — the CA's public half, not a key — so it
	// needs no per-job scoping or rotation, unlike a client certificate
	// would.
	//
	// Only meaningful for the container backend, whose gateway is
	// TLS-secured (https://boid-gateway:<port>). BuildSandboxSpec only
	// writes this into the sandbox (as a file, with GIT_SSL_CAINFO pointed
	// at it) when UsingContainerBackend is true AND spec.Visibility.Clone
	// is set — see its own call site. Without it, every sandbox-internal
	// clone against the gateway fails outright: "server certificate
	// verification failed. CAfile: none CRLfile: none".
	GatewayCAPEM []byte

	// GatewayJobToken is this job's git gateway token, registered against
	// the gateway's Registry at dispatch time (self project fetch/fetch+push,
	// workspace peers and workspace extra_repos fetch-only) and unregistered
	// when the job completes (see Runner.registerGatewayToken /
	// Runner.UnregisterJob). Empty when the gateway isn't wired.
	GatewayJobToken string

	// GatewayCloneURL is the full gateway clone URL for spec's own project
	// (GatewayURL + "/j/" + GatewayJobToken + "/<host>/<owner>/<repo>.git"),
	// built by Runner.buildGatewayCloneURL. Empty unless spec.Visibility.Clone
	// is non-nil (the opt-in sandbox-clone path) — computing it is otherwise
	// wasted work, since nothing would consume it. BuildSandboxSpec only
	// reads this when spec.Visibility.Clone != nil.
	GatewayCloneURL string

	// APIGatewayBaseURL is the shared gateway listener's sandbox-facing base
	// URL. Byte-identical to GatewayURL above in every real deployment
	// — both gateways share the exact same TCP(mTLS) listener — but resolved
	// independently by Runner.registerAPIGatewayToken (which reads r.GatewayURL
	// directly rather than reusing GatewayURL's own local variable) so the
	// API gateway's env-var advertise does not depend on the git gateway
	// having registered anything for this job. Empty when the API gateway
	// isn't wired (r.APIGateway == nil).
	APIGatewayBaseURL string
	// APIGatewayJobToken is this job's API gateway token, registered against
	// apigateway.Registry at dispatch time (floor ∪ workspace enabled-service
	// set, task.readonly → Entry.ReadOnly) and unregistered when the job
	// completes (Runner.registerAPIGatewayToken / Runner.UnregisterJob).
	// Empty when the API gateway isn't wired. BuildSandboxSpec combines this
	// with APIGatewayBaseURL to set BOID_API_BASE.
	APIGatewayJobToken string

	// WorkspacePeerAdvertise is the {name, clone URL, reference path} view of
	// WorkspacePeers, built by Runner.buildPeerAdvertise and keyed by peer
	// project ID; nil when the gateway isn't wired or no peer has a
	// resolvable upstream_url.
	//
	// Unused by BuildSandboxSpec: its sole consumer read the SAME
	// buildPeerAdvertise output through a different carrier —
	// JobContextSnapshot.WorkspacePeerAdvertise (job_context.go), tracked by
	// Runner.Dispatch — not through this SandboxRuntimeInfo field. This
	// field remains genuinely dead; a future cleanup could remove it
	// entirely rather than keep two independent holders of the same
	// buildPeerAdvertise result.
	WorkspacePeerAdvertise map[string]PeerAdvertise

	// CloneWorkspaceDir is the host-side runtime dir path
	// (`<RuntimesDir>/<runtime_id>/workspace`) that BuildSandboxSpec bind-
	// mounts at the sandbox-internal clone target (/workspace/<name>) when
	// spec.Visibility.Clone is set. Allocated and mkdir'd by Runner.Dispatch
	// before BuildSandboxSpec runs, the same way
	// startDockerProxy pre-creates its runtime dir. Empty when RuntimesDir is
	// unset (e.g. minimal test wiring) — cloneMounts then skips the bind and
	// the clone lands on the sandbox's own tmpfs root instead, a safe but
	// non-default degrade (working tree + build artifacts in RAM).
	CloneWorkspaceDir string

	// WorkspaceHomeVolume is the NAME of the docker named volume holding this
	// workspace's persistent home, as resolved by
	// Runner.resolveWorkspaceHome: dockerres.WorkspaceHomeVolumeName(installID,
	// slug), guaranteed to exist — and to have had its bind-target skeleton
	// and, if the workspace declares one, its init.sh run against it — by the
	// time Dispatch reaches BuildSandboxSpec.
	//
	// It is a volume name, NOT a path (internal/sandbox/realization.classifySource
	// treats any non-absolute Source as a named volume) — an earlier
	// design held a host directory path instead, under BOID_RUNTIME_DIR
	// (tmpfs), so a host reboot destroyed every workspace's harness
	// credentials and its ~1.5GB toolchain.
	//
	// The Clone / projectVisible / default HOME branches below (via homeMounts)
	// mount it read-write at HOME's sandbox-internal path instead of a plain
	// tmpfs. env["HOME"] itself is unchanged — it still comes from
	// hostHomeDir(), the *target* path inside the sandbox; only the
	// *contents* now come from the workspace home instead of starting empty
	// every job.
	//
	// When empty (test wiring that never resolved a workspace — most of
	// sandbox_builder_test.go's minimal SandboxRuntimeInfo{} literals —
	// or any other caller that has not threaded a workspace home through
	// yet) the HOME branches gracefully degrade to a single fresh tmpfs.
	// The ProfileInit branch never reads this field at
	// all — see its own doc comment for why bind-mounting HOME there would
	// defeat its host-tool-discovery purpose.
	WorkspaceHomeVolume string

	// WorkspaceSlug is the normalized workspace slug WorkspaceHomeVolume was
	// resolved for, taken straight from resolveWorkspaceHome's second return
	// value by Runner.Dispatch.
	//
	// It must not be re-derived (e.g. via filepath.Base of some path): the
	// home is a named volume called boid-ws-home-<installID8>-<slug>, so
	// deriving the slug from it would put a VOLUME NAME into
	// BOID_WORKSPACE_SLUG and into the adapter error message below — naming
	// a workspace that does not exist to an operator being told which
	// init.sh to edit.
	//
	// BuildSandboxSpec threads it into env["BOID_WORKSPACE_SLUG"] so the
	// claude/codex/opencode adapters' fail-fast "harness CLI not found"
	// error (run.go) can name the exact workspace whose init.sh needs the
	// install step. Empty for test wiring that never resolved a workspace
	// (most of sandbox_builder_test.go's minimal SandboxRuntimeInfo{}
	// literals) — the env var is simply omitted in that case.
	WorkspaceSlug string

	// CloneHostBacked signals that Runner.Dispatch has already materialized
	// CloneWorkspaceDir via dispatcher.PrepareJobCheckout (`git clone
	// file://<bare-repo>` from the project's daemon-managed bare repository
	// into a per-job staging dir under a host-visible runtimes root),
	// rather than leaving CloneWorkspaceDir as an empty scratch directory
	// for the SANDBOX's own in-container clone sequence
	// (buildCloneSpec/performCloneSteps) to populate at job start.
	//
	// Only ever true for a git-URL-registered project (orchestrator.
	// IsBareRepoDir(proj.WorkDir)) dispatched under the container backend
	// with a resolvable host-visible runtimes root — every other caller
	// (a legacy host-dir-registered project, r.RuntimesDir unset test
	// wiring) leaves this false: cloneMounts' /workspace bind stays
	// container-local and buildCloneSpec keeps declaring the in-sandbox
	// clone exactly as before.
	//
	// When true: cloneMounts sets sandbox.Mount.HostBacked on the
	// /workspace bind (realization.classifySource then treats it as a real
	// host-path bind, not container-local — see that field's own doc
	// comment) and buildCloneSpec returns CloneSpec{} (Enabled == false):
	// the sandbox has nothing left to clone, its /workspace/<name> already
	// IS the daemon-prepared checkout.
	CloneHostBacked bool

	// ContainerImage is the workspace's container image override
	// (`orchestrator.WorkspaceMeta.ContainerImage`), resolved by
	// Runner.resolveContainerImage the same way resolveWorkspaceProxy
	// resolves AllowedDomains — an independent WorkspaceLookup.Load call
	// rather than a field threaded through orchestrator.JobSpec, following
	// that field's existing precedent for workspace-level (not
	// project/task-level) dispatch data. Copied verbatim into
	// sandbox.Spec.ContainerImage below; BuildSandboxSpec does not
	// interpret it — only a container backend does. Empty for every
	// workspace that doesn't set container_image (the common case) and for
	// test wiring that never resolved a workspace.
	ContainerImage string
}

// BuildSandboxSpec turns a business-level JobSpec and dispatcher-side runtime
// facts into a primitive sandbox.Spec. It contains no role-aware switch: the
// mount set and environment are derived purely from JobSpec.Visibility,
// HostCommands, Instruction and Argv.
func BuildSandboxSpec(spec *orchestrator.JobSpec, rt SandboxRuntimeInfo) (sandbox.Spec, error) {
	if spec == nil {
		return sandbox.Spec{}, nil
	}

	homeDir := hostHomeDir()
	workDir := resolveWorkDir(spec)
	expandedBindings := expandWorktreeBindings(
		spec.Visibility.AdditionalBindings,
		workDir,
		spec.Visibility.ProjectDir,
	)

	env := cloneStringMap(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	setIfNonEmpty(env, "BOID_TASK_ID", spec.TaskID)
	setIfNonEmpty(env, "BOID_JOB_ID", rt.JobID)
	if inst := spec.Instruction; inst != nil {
		setIfNonEmpty(env, "BOID_MODEL", inst.Model)
		env["BOID_INVOKED_ROLE"] = inst.Role
		env["BOID_INVOKED_NAME"] = inst.Name
		// BOID_INVOKED_BEHAVIOR carries the resolved (canonical) behavior name
		// for the runner / hook scripts. Skill selection no longer branches on
		// this — every task agent bootstraps via /boid-task and determines
		// supervisor/executor mode from `boid task current`'s `readonly` field
		// (the file-based environment.yaml `readonly` this used
		// to read was retired by 5b-4/5b-5). The env var is still exported for
		// legacy run-agent.py and any consumer that wants to log / branch on
		// behavior name.
		// (Previously this exported BOID_INVOKED_TYPE = inst.Type, but that
		// carried the instruction phase — always "execution" — which the runner
		// mistook for a behavior name.)
		if spec.Task != nil {
			env["BOID_INVOKED_BEHAVIOR"] = spec.Task.Behavior
		}
	}
	if spec.Interactive {
		env["BOID_INTERACTIVE"] = "1"
	}
	if _, hasBoid := spec.BuiltinPolicies["boid"]; hasBoid {
		env["BOID_BUILTIN_SHIM"] = "1"
	}
	env["HOME"] = homeDir
	env["TERM"] = "xterm-256color"
	// Defense-in-depth: sandbox 内の git が credential prompt を出して TUI が
	// hang するのを防ぐ。主対策は git-gateway 側の fail-fast だが、以下 2 経路の
	// 401 でも同様に hang しないよう保険を張る:
	//   - gateway 外の upstream 直リンク origin (未移行の workspace の残骸)
	//   - upstream 側で PAT が失効した場合の 401 + WWW-Authenticate: Basic
	// GIT_TERMINAL_PROMPT=0 で prompt 抑止、GIT_ASKPASS=/bin/false で askpass
	// helper 経路もふさぐ。SSH_ASKPASS (別変数) には触らないので、ssh 経路の
	// git は無影響。spec.Env で明示的に上書きされていれば尊重する。
	if _, ok := env["GIT_TERMINAL_PROMPT"]; !ok {
		env["GIT_TERMINAL_PROMPT"] = "0"
	}
	if _, ok := env["GIT_ASKPASS"]; !ok {
		env["GIT_ASKPASS"] = "/bin/false"
	}
	// Resolve adapter bindings once. When HarnessType identifies a known
	// adapter (claude/codex/opencode) its Bindings() take the place of the
	// kit-declared additional_bindings — the kit-free dispatch path. For
	// unknown harnesses we fall back to kit-declared bindings below.
	var harnessBindings []orchestrator.BindMount
	if a := registry.For(sandbox.HarnessType(spec.HarnessType)); a != nil {
		harnessBindings = adapterBindingsToOrchestrator(a.Bindings(homeDir))
	}
	// adapter-driven bindings は adapter が non-nil Bindings() を返したときだけ
	// 採用する。 spec.HarnessType != "" だけで分岐すると shell adapter
	// (Bindings()=nil) のとき pathBindings/mounts が空に潰れ、 kit 由来の
	// spawn.sh / additional_bindings が sandbox に bind されず hook script
	// が見えなくなる。 shell adapter は legacy kit binding 経路に乗せたい。
	pathBindings := expandedBindings
	if len(harnessBindings) > 0 {
		pathBindings = harnessBindings
	}
	env["PATH"] = buildPATH(pathBindings)
	if rules := buildHostCommandRulesEnv(rt.ResolvedHostCommandsByName); rules != "" {
		env[sandbox.HostCommandRulesEnv] = rules
	}
	env["BOID_HOST_IP"] = hostGatewayIP
	setIfNonEmpty(env, "BOID_WORKSPACE_SLUG", rt.WorkspaceSlug)
	if rt.ProxyPort > 0 {
		noProxyExtra := append([]string{gatewayHostFromURL(rt.GatewayURL)}, rt.WorkspaceNetworkCIDRs...)
		applyProxyEnv(env, rt.ProxyHost, rt.ProxyPort, noProxyExtra...)
	}
	if rt.ProxySocketPath != "" {
		applyDockerProxyEnv(env)
	}

	var mounts []sandbox.Mount
	var files []sandbox.FileWrite

	// Broker socket + token.
	if rt.BrokerSocket != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: rt.BrokerSocket,
			Target: "/run/boid/broker.sock",
			Type:   sandbox.MountBind,
			IsFile: true,
		})
		env["BOID_BROKER_SOCKET"] = "/run/boid/broker.sock"
	}
	if rt.BrokerToken != "" {
		env["BOID_BROKER_TOKEN"] = rt.BrokerToken
	}

	// Project / workspace peers / .boid layer.
	projectDir := spec.Visibility.ProjectDir
	switch {
	case spec.SandboxProfile == int(sandbox.ProfileInit):
		// ProfileInit (boid kit init / workspace configure): the plan rbinds the
		// entire host root read-only precisely so the scan can discover host
		// state, and most of the interesting tooling lives under HOME
		// (`~/.volta/bin/volta`, `~/.local/bin/go`, `~/.nvm/versions/...`, ...).
		// Layering a full HOME tmpfs — or the workspace HOME volume — on top
		// would shadow exactly those paths and make `which volta` / `ls
		// ~/.volta/bin` return nothing, defeating the whole point of
		// ProfileInit. Layer a tmpfs over `<HOME>/.boid` only so any script
		// write under $HOME/.boid/* still lands on writable storage without
		// hiding the rest of HOME. ProfileInit jobs get no broker socket (see
		// Runner.Dispatch), so they were never a payload_patch.json
		// producer/consumer to begin with — this mount is unrelated to
		// homeMounts' own payload-patch handling below.
		//
		// FIRST in this switch, ahead of the visibility arms. The profile
		// describes what the job DOES (scan the
		// host) while Visibility is assembled independently by whoever builds
		// the JobSpec, so nothing structurally keeps a ProfileInit job from
		// also carrying a ProjectDir or a Clone declaration. Ordered the other
		// way round, either of those took HOME back — through
		// projectVisibilityMounts' own homeMounts step, or the Clone arm's —
		// and the contract this branch and homeSkeleton both state ("ProfileInit
		// never mounts the workspace home; $HOME there is the host's own")
		// held only for the profile in isolation. Nothing is lost by winning
		// here: under a read-only rbind of the whole host root the project is
		// already visible at its own path, and a directory that must be
		// WRITABLE is expressed as an rw additional binding, which is applied
		// below regardless of which arm ran (see additionalBindingMounts'
		// Source==Target rw carve-out, written for exactly this profile).
		//
		// The tmpfs target must exist on the host (mounts cannot create their
		// own mountpoint), so make sure `<HOME>/.boid` is present before the
		// runner pivots in. The daemon process runs as the same uid that owns
		// `<HOME>`, so the mkdir succeeds without elevation.
		if err := os.MkdirAll(homeDir+"/.boid", 0o755); err != nil {
			return sandbox.Spec{}, fmt.Errorf("ensure %s/.boid: %w", homeDir, err)
		}
		mounts = append(mounts, sandbox.Mount{
			Target: homeDir + "/.boid",
			Type:   sandbox.MountTmpfs,
		})
	case spec.Visibility.Clone != nil:
		// Sandbox-clone path: skip projectVisibilityMounts entirely.
		// cloneMounts (below) mounts the reference `.git` dirs and the clone
		// target at the neutral /workspace path; there is no host ProjectDir
		// bind for this job at all, so binding projectDir here too would
		// double-mount the same host path at two sandbox targets for no
		// reason. HOME still gets the workspace home bind or a private tmpfs
		// fallback, exactly like the "no project visible" case below.
		mounts = append(mounts, homeMounts(homeDir, rt.WorkspaceHomeVolume)...)
	case projectDir != "":
		mounts = append(mounts, projectVisibilityMounts(
			projectDir,
			projectDir,
			homeDir,
			rt.WorkspaceHomeVolume,
			spec.Visibility.Writable,
			rt.WorkspacePeers,
		)...)
	default:
		// No project visible: HOME gets the workspace home bind (+ the
		// embedded-skill binds) or a fresh tmpfs fallback, same as the Clone
		// case above.
		mounts = append(mounts, homeMounts(homeDir, rt.WorkspaceHomeVolume)...)
	}

	// Sandbox-internal clone mounts: RO bind of the host project `.git`
	// (for `git clone --reference`) and
	// the workspace peers' `.git` dirs, plus a real (non-shimmed) git binary
	// the runner's own clone/branch-resolution invocations use. Entirely
	// opt-in: nil unless spec.Visibility.Clone is set, so the existing
	// worktree/project mount layout above is completely unaffected.
	mounts = append(mounts, cloneMounts(spec, rt)...)

	// Git gateway / API gateway TLS trust: only the container backend's
	// gateway URL is TLS-secured (see SandboxRuntimeInfo.GatewayCAPEM's own
	// doc comment) — a job with no clone declared AND no API gateway token
	// needs nothing here (it never talks to the gateway at all). Written as a
	// plain sandbox file (not a mount): the CA cert is non-secret,
	// daemon-lifetime-static content, exactly like other spec.Files
	// entries, not a per-job artifact that needs its own mount/cleanup
	// lifecycle.
	needsGatewayCA := rt.UsingContainerBackend && len(rt.GatewayCAPEM) > 0 &&
		(spec.Visibility.Clone != nil || rt.APIGatewayJobToken != "")
	if needsGatewayCA {
		files = append(files, sandbox.FileWrite{
			Path:    containerGitGatewayCAPath,
			Content: string(rt.GatewayCAPEM),
		})
	}
	if spec.Visibility.Clone != nil && rt.UsingContainerBackend && len(rt.GatewayCAPEM) > 0 {
		env["GIT_SSL_CAINFO"] = containerGitGatewayCAPath
	}

	// BOID_API_BASE: the base URL a job
	// prefixes onto a service-relative path to reach the API gateway
	// (`curl "$BOID_API_BASE/myapp/v1/users"`) — job-token-scoped, minted by
	// Runner.registerAPIGatewayToken. Empty (both fields) when the API
	// gateway isn't wired (r.APIGateway == nil) or this job's Runner.Dispatch
	// call didn't register a token for some other reason.
	//
	// TLS trust caveat: under the container backend,
	// BOID_API_BASE is an https:// URL secured by the daemon's own internal
	// CA, which no client trusts by default — a bare `curl "$BOID_API_BASE/..."`
	// (the plan doc's own headline example) fails TLS verification unless
	// the caller ALSO passes the CA explicitly. The env vars below narrow
	// this gap as far as it safely can be narrowed without either requiring
	// every caller to know a boid-specific flag or silently breaking TLS
	// trust for every OTHER https host this sandbox talks to — see each
	// var's own comment for why it is or isn't included.
	if rt.APIGatewayJobToken != "" && rt.APIGatewayBaseURL != "" {
		env["BOID_API_BASE"] = rt.APIGatewayBaseURL + apigateway.PathPrefix + rt.APIGatewayJobToken
		if needsGatewayCA {
			// BOID_API_CA_FILE: an explicit, OPT-IN CA path for tools that
			// accept one directly (curl --cacert "$BOID_API_CA_FILE", Python
			// requests(..., verify=os.environ["BOID_API_CA_FILE"]), etc.).
			// This remains the ONLY mechanism for such tools — a bare `curl
			// "$BOID_API_BASE/..."` with no flag still fails TLS verification
			// for them, rather than overclaiming a truly flagless
			// "any SDK, base URL swap only" experience.
			env["BOID_API_CA_FILE"] = containerGitGatewayCAPath
			// NODE_EXTRA_CA_CERTS: unlike SSL_CERT_FILE / CURL_CA_BUNDLE
			// (both REPLACE a tool's default trust store when set — setting
			// either to just this CA would silently break TLS verification
			// for pypi.org/github.com/etc. from that tool's perspective),
			// Node.js documents this variable as ADDITIVE: "the well known
			// 'root' CAs... will be extended with the extra certificates in
			// file" (Node.js CLI docs on NODE_EXTRA_CA_CERTS). A Node-based
			// SDK/script therefore gets genuine flagless "curl-equivalent"
			// TLS trust for BOID_API_BASE, with no loss of trust for any
			// other host Node talks to — PROVIDED this doesn't clobber a
			// value the project/workspace already declared for its OWN
			// reasons: unlike BOID_API_CA_FILE
			// (a name boid invents — nothing could have set it before),
			// NODE_EXTRA_CA_CERTS is a real, pre-existing Node.js variable a
			// project.yaml/workspace `env:` block may already point at a
			// DIFFERENT CA bundle (e.g. a corporate proxy or internal
			// registry Node itself talks to, unrelated to this gateway).
			// Env already carries spec.Env's fully-merged value by this point
			// (env := cloneStringMap(spec.Env) at BuildSandboxSpec's top), so
			// only set this when the caller hasn't already claimed it — a
			// caller who already needs it for something else keeps their own
			// value and simply doesn't get the automatic Node trust
			// (BOID_API_CA_FILE remains available for them to combine by
			// hand). curl/Python/other openssl-backed tools have no
			// equivalent safe (additive) env var, so they remain on the
			// explicit BOID_API_CA_FILE opt-in unconditionally.
			if _, alreadySet := env["NODE_EXTRA_CA_CERTS"]; !alreadySet {
				env["NODE_EXTRA_CA_CERTS"] = containerGitGatewayCAPath
			}
		}
	}

	// Additional bindings:
	//   * The harness adapter (claude / codex / opencode) declares the
	//     agent-CLI bindings it needs (~/.claude, ~/.local/bin, ...). Those
	//     go in directly.
	//   * On top, project.yaml-declared additional_bindings carry
	//     environment-specific tooling paths (~/.volta, ~/.nuget, /opt/google/
	//     chrome, /usr/lib/dotnet, ...). The original Phase 3-c "kit-free
	//     dispatch path" used to drop these on the assumption that kits only
	//     existed in boid-kits and only supplied agent CLI plumbing — but the
	//     2026-06-26 workspace+kit reorg made kits a per-user place to declare
	//     host-side tool bindings, so they must apply on top of harness
	//     bindings rather than be replaced by them.
	mounts = append(mounts, additionalBindingMounts(harnessBindings)...)
	mounts = append(mounts, additionalBindingMounts(expandedBindings)...)

	// Server socket (exec jobs that need to talk to boid daemon).
	//
	// ProfileInit (boid kit init / workspace configure) は host `/` を read-only
	// rbind しているので、 /run/boid/server.sock を target にすると applyMount の
	// MkdirAll が /run/boid を ro な /run 配下に作ろうとして EPERM になる
	// (host 側に /run/boid ディレクトリは通常存在しない — daemon socket は
	// /run/user/<uid>/boid.sock 等)。 host root rbind が socket をすでに host
	// 側 path 経由で露出しているので、 ProfileInit では追加 bind を張らず
	// BOID_SOCKET だけ host path に向ける。 通常 profile (task/exec) ではこれま
	// で通り /run/boid/server.sock に bind する。
	if rt.ServerSocket != "" {
		if spec.SandboxProfile == int(sandbox.ProfileInit) {
			env["BOID_SOCKET"] = rt.ServerSocket
		} else {
			mounts = append(mounts, sandbox.Mount{
				Source: rt.ServerSocket,
				Target: "/run/boid/server.sock",
				Type:   sandbox.MountBind,
				IsFile: true,
			})
			env["BOID_SOCKET"] = "/run/boid/server.sock"
		}
	}

	// Docker proxy socket (per-sandbox docker proxy for capabilities.docker).
	if rt.ProxySocketPath != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: rt.ProxySocketPath,
			Target: dockerProxySandboxSocket,
			Type:   sandbox.MountBind,
			IsFile: true,
		})
	}

	argv := append([]string(nil), spec.Argv...)

	// stdin / stdout routing.
	//
	// Interactive jobs must inherit the PTY on stdin/stdout — piping PrimaryInput
	// via `printf | argv` or redirecting stdout to a capture file would break
	// isatty() detection in TUIs and force them into
	// non-interactive mode. Interactive hook agents read PrimaryInput via the
	// `boid task payload` broker RPC rather than stdin, and the runner's
	// broker job-done reads the result from this stdout-capture file —
	// see resolveJobOutput's doc comment.
	var stdinBytes []byte
	if !spec.Interactive && len(spec.PrimaryInput) > 0 {
		stdinBytes = append(stdinBytes, spec.PrimaryInput...)
	}
	// stdout capture is a batch pattern: the leaf command's stdout is
	// redirected to a sandbox-internal file and read back after the process
	// exits (postJobDone's resolveJobOutput fallback), never streamed live.
	// That is exactly right for hook jobs (headless, nobody is watching in
	// real time) but wrong for `boid exec`: the whole point of the git
	// gateway cutover's Dispatch() migration is that exec now runs through
	// the same LocalRuntime pipe/PTY transport as a session job, and its
	// live output must reach the CLI's attach stream, not a file nobody
	// reads until completion. So JobKindExec is excluded regardless of
	// Interactive — see dispatcher.BuildExecJobSpec / runtime_local_linux.go's
	// non-interactive branch, which now streams stdout+stderr through the
	// plain pipe transport for this exact case.
	var stdoutCapture string
	if !rt.Foreground && !spec.Interactive && spec.Kind != orchestrator.JobKindExec {
		stdoutCapture = "/tmp/boid-output"
	}

	// boid binary bind + host command shims.
	//
	// The git-shim PATH overlay (/usr/bin/git, /bin/git bound to the boid
	// binary) was retired: sandbox git is now always the real binary
	// visible via the base rbind of /usr — every job clones inside the
	// sandbox rather than sharing a host worktree, so there is no shared
	// `.git` for a sandbox-side git invocation to escape through and no
	// reason to route git through the broker any more. The broker-side git
	// builtin and its "git" BuiltinPolicy registration were subsequently
	// deleted too.
	//
	// The host-command shim scheme uses a fixed
	// directory (sandboxShimBinDir) that holds the boid multi-call binary
	// once and a symlink per host command name pointing at it — replacing
	// an earlier "bind boid at each host command's absolute host path"
	// scheme (hostCommandMounts, retired). Every shim's
	// bind-mount basename now equals its declared short name by construction,
	// so the retired BOID_HOST_COMMAND_NAMES env-map lookup and the broker's
	// Path-scan fallback both become dead weight; both were dropped in the
	// same change. ProfileInit is excluded — its host `/` rbind already
	// exposes the boid binary at its real path and it declares no host
	// commands, so no shim is needed.
	var symlinks []sandbox.Symlink
	if rt.BoidBinary != "" && spec.SandboxProfile != int(sandbox.ProfileInit) {
		// UsingContainerBackend (see its own doc comment):
		// the container backend's shared image already bakes boid at this
		// exact shim path (build/container/Dockerfile), so binding
		// rt.BoidBinary again would try to bind-mount the DAEMON's own
		// in-image path as a DooD sibling mount source, which the host's
		// real docker daemon rejects outright. Individual host-command
		// shim symlinks are NOT baked and must still be generated
		// for both backends — only this bind mount is skipped for the
		// container backend.
		if !rt.UsingContainerBackend {
			mounts = append(mounts, sandbox.Mount{
				Source:   rt.BoidBinary,
				Target:   sandboxShimBinDir + "/boid",
				Type:     sandbox.MountBind,
				IsFile:   true,
				ReadOnly: true,
			})
		}
		symlinks = hostCommandSymlinks(rt.ResolvedHostCommandsByName)
	}

	tty := spec.Interactive

	// Resolve harness-specific extras before assembling the Spec. For
	// HarnessType=="claude" the runner hands the agent off to
	// internal/adapters/claude.Adapter.Run(), so the runner needs the
	// user-answer threaded through and the spec needs to advertise the
	// harness type.
	var harness sandbox.HarnessType
	var userAnswer string
	if spec.HarnessType != "" {
		harness = sandbox.HarnessType(spec.HarnessType)
		userAnswer = spec.Env["BOID_USER_ANSWER"]
	}

	homeSkeletonRoot, homeSkeletonDirs := homeSkeleton(homeDir, rt, mounts)

	out := sandbox.Spec{
		ID:                rt.JobID,
		Mounts:            mounts,
		Files:             files,
		Symlinks:          symlinks,
		ProxyPort:         rt.ProxyPort,
		Argv:              argv,
		WorkDir:           workDir,
		Env:               env,
		StdinBytes:        stdinBytes,
		StdoutCaptureFile: stdoutCapture,
		TTY:               tty,
		// Foreground jobs (boid exec) get no broker job-done; hook jobs leave it
		// false so runner-inner-child posts `boid job done` on agent exit. The
		// runner reads the result from the stdout-capture file — agents /
		// hook scripts apply their payload patch immediately via the
		// broker's `boid task update --payload-patch` RPC.
		Foreground:     rt.Foreground,
		HarnessType:    harness,
		UserAnswer:     userAnswer,
		Profile:        sandbox.Profile(spec.SandboxProfile),
		Clone:          buildCloneSpec(spec, rt),
		ContainerImage: rt.ContainerImage,
		// The bind-target skeleton the job's own runner verifies before
		// starting the harness — see sandbox.Spec.HomeSkeletonDirs and
		// runner.verifyHomeSkeleton for why the check moved into the
		// container). Both values are derived from the mount list assembled
		// above rather than from rt's fields, so nothing here can name a
		// directory the check cannot actually observe — see homeSkeleton.
		HomeSkeletonRoot: homeSkeletonRoot,
		HomeSkeletonDirs: homeSkeletonDirs,
	}
	return out, nil
}

// homeSkeleton decides what the JOB CONTAINER's own runner is asked to verify
// about the workspace HOME before it starts the harness: the root the entries
// are relative to (the sandbox $HOME), and the entries themselves — the subset
// of workspaceHomeSkeletonDirs this sandbox can actually observe. Both are
// zero when there is nothing to check.
//
// # The answer comes from the MOUNT LIST, not from the runtime info
//
// The tempting gate is "rt.WorkspaceHomeVolume is set", and it is not enough:
// a ProfileInit sandbox (`boid kit init` / workspace configure) HAS that field
// — Runner.Dispatch resolves the workspace home before it knows the profile —
// yet the ProfileInit branch of the mount switch deliberately never mounts it.
// $HOME there is the host's own, rbind-mounted read-only. Every entry would
// then be a directory nothing was going to create, and the runner would fail
// every such job.
//
// Reading the assembled mounts answers that, and keeps answering it if the
// layout changes: a mount laid over .claude (or .agents) would make THAT
// directory unobservable — os.Stat would read the mount SOURCE rather than the
// directory inside the workspace HOME volume this check is about — and it
// drops out here rather than turning into an assertion about someone else's
// filesystem. That failure was real while the per-skill leaves were covered by
// read-only binds: every leaf passed unconditionally, on evidence about a
// different directory, while this function's doc comment and its tests claimed
// a set it never covered. Those binds are gone
// and the skeleton no longer has leaves, so nothing shadows an entry today —
// the filter stays because it is what makes that true rather than something
// this function assumes.
func homeSkeleton(homeDir string, rt SandboxRuntimeInfo, mounts []sandbox.Mount) (root string, dirs []string) {
	if rt.WorkspaceHomeVolume == "" {
		return "", nil
	}
	covered := make(map[string]struct{}, len(mounts))
	homeMounted := false
	for _, m := range mounts {
		covered[m.Target] = struct{}{}
		if m.Target == homeDir && m.Source == rt.WorkspaceHomeVolume {
			homeMounted = true
		}
	}
	if !homeMounted {
		return "", nil
	}
	for _, rel := range workspaceHomeSkeletonDirs() {
		if _, shadowed := covered[filepath.Join(homeDir, rel)]; shadowed {
			continue
		}
		dirs = append(dirs, rel)
	}
	if len(dirs) == 0 {
		return "", nil
	}
	return homeDir, dirs
}

// resolveWorkDir returns the initial cd target inside the sandbox. The
// sandbox-clone opt-in path (spec.Visibility.Clone != nil) takes priority
// over the plain project-dir path since its bind mount above never exposes
// ProjectDir at all — the only filesystem the clone-mode sandbox has is the name-scoped
// subdirectory of sandboxCloneTargetDir (see sandboxCloneDir). Otherwise
// prefer the project dir, then home.
func resolveWorkDir(spec *orchestrator.JobSpec) string {
	if spec.Visibility.Clone != nil {
		return sandboxCloneDir(cloneDirNameForVisibility(spec.Visibility))
	}
	if spec.Visibility.ProjectDir != "" {
		return spec.Visibility.ProjectDir
	}
	return hostHomeDir()
}

// sandbox-internal neutral paths used by the opt-in clone sequence.
// Fixed rather than derived so the
// runner (which reads sandbox.CloneSpec back out of the JSON spec file) and
// dispatcher (which generates the matching mounts) always agree without
// having to thread the values through some other channel.
const (
	// sandboxCloneTargetDir is the neutral clone destination's *parent*
	// directory: every job actually clones into a per-project subdirectory
	// of this parent — sandboxCloneDir(name) — rather than directly at this
	// path. Two problems motivate the parent-dir split: (1) every project
	// sharing the exact same absolute sandbox cwd ("/workspace") would make
	// Claude Code's `~/.claude/projects/-workspace/` session-log slug
	// collide across every boid project; (2) an agent dynamically cloning a
	// workspace peer would have no obvious place to put it other than
	// $HOME or /tmp (both tmpfs, RAM-backed) — /workspace/<peer> is the
	// natural spot once /workspace is a parent dir. See PeerAdvertise.CloneDir
	// for how a peer learns its own suggested directory name. The self
	// project has no equivalent advertised field — the sandbox actually
	// `cd`s the agent there, so `pwd` is the only source of truth for its
	// own project's directory.
	sandboxCloneTargetDir = "/workspace"

	// sandboxCloneReferenceDir is where the host project's `.git` is RO
	// bind-mounted for use as `git clone --reference`.
	sandboxCloneReferenceDir = "/mnt/refs/self.git"

	// sandboxClonePeerReferenceDirFmt is the Sprintf pattern (keyed by peer
	// project ID) for RO bind-mounting workspace peers' `.git` dirs.
	// Dynamic peer clone is later work; this only makes the mounts
	// constructible today.
	sandboxClonePeerReferenceDirFmt = "/mnt/refs/peers/%s.git"

	// containerGitGatewayCAPath is the fixed sandbox-internal path SandboxRuntimeInfo.GatewayCAPEM
	// is written to (as a plain spec.Files entry, container backend +
	// clone-visibility-or-API-gateway-token jobs only) and what
	// GIT_SSL_CAINFO / BOID_API_CA_FILE point at, so the sandbox-internal
	// `git clone` (or an arbitrary curl/SDK call against BOID_API_BASE)
	// against the container backend's TLS-secured gateway
	// (https://boid-gateway:<port>) can verify the gateway's server
	// certificate.
	//
	// Deliberately under /run/boid/bin, not /run/boid itself: the shared
	// image (build/container/Dockerfile) only `chgrp -R 0` + `chmod -R
	// g=u` on /run/boid/bin, for arbitrary-uid support — /run/boid, its
	// parent, stays root-owned (mode 0755 from
	// the preceding `mkdir -p /run/boid/bin`, which creates both in one
	// step but the permission fix-up only covers the child).
	// writeFileAt's own os.MkdirAll(filepath.Dir(path)) is a no-op
	// whenever the target directory already exists (regardless of its
	// ownership), so a path directly under /run/boid — the first version
	// of this fix used exactly that — creates the file's *parent*
	// successfully (already present) but then fails the actual
	// os.WriteFile with EACCES: the non-root job container (--user
	// b.uid:b.gid) cannot create a new directory entry in a root-owned,
	// group/other-non-writable directory. /run/boid/bin has no such
	// problem — it is the one directory under /run/boid this image
	// deliberately makes group-0-writable specifically so the non-root
	// entrypoint can write into it (see the Dockerfile's own comment on
	// that RUN step) — reusing it here for a second kind of per-container
	// artifact (a CA cert alongside the host-command shim symlinks) is a
	// directory-choice convenience, not a semantic host-command binding.
	containerGitGatewayCAPath = "/run/boid/bin/gitgateway-ca.crt"
)

// sandboxCloneDir returns the absolute sandbox-internal directory a project
// actually clones into: sandboxCloneTargetDir ("/workspace") plus the
// resolved leaf name, e.g. "/workspace/bm-next". name is expected to come from
// projectDirName, which never returns empty for a project with a non-empty
// WorkDir — but an empty name here (Projects unwired, or some other
// resolution failure upstream) degrades gracefully to the bare parent dir
// itself, reproducing the flat "/workspace" layout instead of
// producing a malformed path like "/workspace/" or panicking.
//
// Defensive filter: a name that is empty, ".", or contains
// a path separator / NUL byte / ".." prefix is treated as unusable and
// falls back to the bare parent dir. project.yaml's `meta.name` is
// user-authored so the trust boundary is loose, but an accidental "../" or
// "/" would escape /workspace entirely — the defensive branch turns any
// such name into the same graceful degrade as an empty name (no
// /workspace/.. clone escape, no /workspace/. no-op subdir). See also
// `isSafeCloneDirName`.
func sandboxCloneDir(name string) string {
	if !isSafeCloneDirName(name) {
		return sandboxCloneTargetDir
	}
	return sandboxCloneTargetDir + "/" + name
}

// isSafeCloneDirName reports whether name is a usable single-segment leaf
// directory name under sandboxCloneTargetDir. It rejects empty / "." / ".."
// / any name containing a path separator or NUL, and any name starting with
// "..". `project.yaml`'s `meta.name` is trusted (user-authored config,
// convention: kebab-case) so this is a defense-in-depth filter rather than
// a security boundary — its job is to keep an accidental typo or a stray
// filepath.Base call ("." for an empty path) from producing a malformed
// clone target like `/workspace/.` or `/workspace/../etc`.
func isSafeCloneDirName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\x00") {
		return false
	}
	if strings.HasPrefix(name, "..") {
		return false
	}
	return true
}

// projectDirName resolves the leaf directory name a project's sandbox clone
// lands under (workspace 親化リファクタリング, nose 2026-07-13 decision):
// project.Name when set (expected kebab-case by convention, not enforced
// here), falling back to filepath.Base(workDir) when the project has no
// name. Shared by the self-project resolution (cloneDirNameForVisibility)
// and the workspace-peer resolution (Runner.buildPeerAdvertise).
//
// filepath.Base("") returns ".", so an empty workDir would leak "." here;
// projectDirName intentionally returns "" in that case instead so the
// downstream sandboxCloneDir defensive filter degrades cleanly to the bare
// parent dir rather than emitting "/workspace/.".
func projectDirName(name, workDir string) string {
	if name != "" {
		return name
	}
	if workDir == "" {
		return ""
	}
	return filepath.Base(workDir)
}

// cloneDirNameForVisibility resolves the leaf directory name spec's own
// project clones into under the sandbox's /workspace parent dir. v.
// ProjectName is business data threaded through JobSpec.Visibility by
// orchestrator.PlanHook / dispatcher.BuildSessionJobSpec — both already read
// the workspace-hydrated ProjectMeta.Name at JobSpec-build time (see
// orchestrator.Visibility.ProjectName's doc comment for why that is the
// correct place to resolve it, rather than a second, dispatcher-side
// Projects.GetProject lookup). v.ProjectDir is the same host path already
// used everywhere else in this file.
func cloneDirNameForVisibility(v orchestrator.Visibility) string {
	return projectDirName(v.ProjectName, v.ProjectDir)
}

// cloneMounts returns the mounts for the opt-in sandbox-clone path: the RO
// reference-repo binds (self + workspace peers) used for `git clone
// --reference`, plus the host-backed /workspace bind the clone actually
// lands on. Returns nil (no mounts) unless spec.Visibility.Clone is set, so
// the default dispatch path's mount list is completely unaffected.
//
// There is no separate real-git-binary bind: the git-shim overlay (/usr/bin/git,
// /bin/git bound to the boid binary) is retired (see the "boid binary bind
// + host command mounts" section below), so the sandbox's own
// /usr/bin/git — visible via the base rbind of /usr — is already the real
// binary; performClone's bare "git" $PATH lookup resolves correctly with no
// extra mount needed.
func cloneMounts(spec *orchestrator.JobSpec, rt SandboxRuntimeInfo) []sandbox.Mount {
	if spec == nil || spec.Visibility.Clone == nil {
		return nil
	}
	var out []sandbox.Mount

	if projectDir := spec.Visibility.ProjectDir; projectDir != "" {
		gitDir := projectDir + "/.git"
		out = append(out, sandbox.Mount{
			Source:     gitDir,
			Target:     sandboxCloneReferenceDir,
			Type:       sandbox.MountBind,
			ReadOnly:   true,
			DetectType: true,
			Guard:      existsGuardExpr(gitDir),
		})
	}

	peerIDs := make([]string, 0, len(rt.WorkspacePeers))
	for id := range rt.WorkspacePeers {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)
	for _, id := range peerIDs {
		gitDir := rt.WorkspacePeers[id] + "/.git"
		out = append(out, sandbox.Mount{
			Source:     gitDir,
			Target:     fmt.Sprintf(sandboxClonePeerReferenceDirFmt, id),
			Type:       sandbox.MountBind,
			ReadOnly:   true,
			DetectType: true,
			Guard:      existsGuardExpr(gitDir),
		})
	}

	// /workspace bind: the temp area's substance is a bind mount of the host
	// runtime dir by default. rt.CloneWorkspaceDir is a fresh, job-scoped
	// `<RuntimesDir>/<runtime_id>/workspace` directory Runner.Dispatch
	// pre-creates, so it already exists on the host before the mount is
	// applied — no DetectType/Guard needed, unlike the reference-dir binds
	// above (whose host source may legitimately be absent, e.g. a project
	// with no peers). Always read-write: under the clone model readonly is
	// enforced by the gateway (transport-RO), not the local filesystem.
	// Empty CloneWorkspaceDir (e.g. RuntimesDir unset in minimal test
	// wiring) skips the bind — the clone then simply lands on the sandbox's
	// own tmpfs root, a safe non-default degrade.
	if rt.CloneWorkspaceDir != "" {
		out = append(out, sandbox.Mount{
			Source: rt.CloneWorkspaceDir,
			Target: sandboxCloneDir(cloneDirNameForVisibility(spec.Visibility)),
			Type:   sandbox.MountBind,
			// HostBacked: see SandboxRuntimeInfo.CloneHostBacked's own doc
			// comment. False keeps the container backend's existing
			// container-local classification for this target unchanged —
			// only a daemon-pre-populated staging area opts into a real
			// host-path bind.
			HostBacked: rt.CloneHostBacked,
		})
	}

	return out
}

// realGitBinPath resolves the host's real git binary path via $PATH. Kept
// as a dispatch-time diagnostic: git is a hard dependency for every
// sandbox-internal clone (the sandbox's own /usr/bin/git is an rbind of the
// daemon host's), so a LookPath failure here — meaning the daemon host has
// no git on PATH at all — is surfaced loudly rather than silently papered
// over. Nothing binds the
// returned path anywhere anymore; buildCloneSpec calls this purely for the
// warning side effect.
func realGitBinPath() string {
	if p, err := exec.LookPath("git"); err == nil {
		return p
	}
	slog.Warn("realGitBinPath: git not found on daemon host PATH; sandbox-internal clone will fail once dispatched")
	return "/usr/bin/git"
}

// buildCloneSpec translates spec.Visibility.Clone (the orchestrator-level
// declaration) plus dispatcher-resolved runtime facts (rt.GatewayCloneURL)
// into the sandbox.CloneSpec the runner consumes. Returns the zero value
// (Enabled == false) when spec.Visibility.Clone is nil — see CloneSpec's own
// doc comment for why that is a complete no-op for the runner.
//
// Also returns the zero value when rt.CloneHostBacked is true:
// Runner.Dispatch has already cloned
// and checked out the right branch into CloneWorkspaceDir via
// dispatcher.PrepareJobCheckout BEFORE the job container ever starts, and
// cloneMounts' matching HostBacked bind makes that staging dir /workspace/
// <name> itself — the sandbox has nothing left to clone. Leaving
// Enabled == true here for a host-backed job would make
// internal/sandbox/runner/clone.go's performCloneSteps wipe and re-clone
// the daemon's own pre-populated checkout via the git-gateway HTTP reverse
// proxy on every dispatch, defeating the entire point of pre-cloning.
func buildCloneSpec(spec *orchestrator.JobSpec, rt SandboxRuntimeInfo) sandbox.CloneSpec {
	if spec == nil || spec.Visibility.Clone == nil || rt.CloneHostBacked {
		return sandbox.CloneSpec{}
	}
	realGitBinPath() // dispatch-time warning only; see doc comment above.
	cd := spec.Visibility.Clone
	return sandbox.CloneSpec{
		Enabled:             true,
		URL:                 rt.GatewayCloneURL,
		ReferenceDir:        sandboxCloneReferenceDir,
		TargetDir:           sandboxCloneDir(cloneDirNameForVisibility(spec.Visibility)),
		Branch:              cd.Branch,
		BaseBranch:          cd.BaseBranch,
		CheckoutOnly:        cd.CheckoutOnly,
		BaseBranchForkPoint: cd.BaseBranchForkPoint,
	}
}

// homeMounts returns the HOME mount(s) for a sandbox. When workspaceHomeVolume
// is non-empty, HOME becomes a read-write mount of the workspace's
// persistent home. When it is empty (test wiring that never resolved a
// workspace home, or any caller that has not threaded
// SandboxRuntimeInfo.WorkspaceHomeVolume through yet) this degrades
// gracefully to a single fresh tmpfs at homeDir.
//
// # The HOME mount is a named VOLUME, spelled as a MountBind
//
// workspaceHomeVolume is a docker volume NAME, and the sandbox.Mount that
// carries it is still Type: MountBind. That is not an oversight: the
// realization layer decides bind-vs-volume from the SOURCE, not the type —
// internal/sandbox/realization.classifySource returns MountSourceNamedVolume
// for any Source that does not begin with "/", and containerMounts turns
// that into mount.TypeVolume.
//
// It layers nothing on top of the home: the embedded skill set and every
// loaded Integration Pack's reach the home by symlink instead, written once
// by the init container's prelude (skills_overlay.go) rather than by a bind
// mount here.
//
// Shared by the Clone branch, the default (no-project) branch and
// projectVisibilityMounts's HOME step below so all three switch over
// identically.
func homeMounts(homeDir, workspaceHomeVolume string) []sandbox.Mount {
	if workspaceHomeVolume == "" {
		// No workspace home resolved: a plain private tmpfs, with no skills
		// layered on. resolveWorkspaceHome never returns an empty directory
		// on a real dispatch (it mkdir's the home or fails), so this branch
		// is test wiring only and there is no behaviour here worth changing.
		return []sandbox.Mount{{
			Target: homeDir,
			Type:   sandbox.MountTmpfs,
		}}
	}
	return []sandbox.Mount{
		{
			Source: workspaceHomeVolume,
			Target: homeDir,
			Type:   sandbox.MountBind,
		},
	}
}

// projectVisibilityMounts returns the canonical mount layout that lets the
// sandbox see the project and workspace peers, under a HOME mount (workspace
// home bind, or a tmpfs fallback — see homeMounts) that shadows host files
// but re-mounts the project on top.
func projectVisibilityMounts(
	origProjectDir, effectiveDir, homeDir, workspaceHomeVolume string,
	writable bool,
	peers map[string]string,
) []sandbox.Mount {
	var out []sandbox.Mount

	// 1) bind the effective dir (= project or worktree)
	out = append(out, sandbox.Mount{
		Source:   effectiveDir,
		Target:   effectiveDir,
		Type:     sandbox.MountBind,
		ReadOnly: !writable,
	})

	// 2) HOME mount(s) on top of user's home (isolates config files from
	// host): a workspace home bind, or a fresh tmpfs fallback when no
	// workspace home is resolved.
	out = append(out, homeMounts(homeDir, workspaceHomeVolume)...)

	// 3) re-mount the effective dir so the HOME mount (tmpfs or workspace
	// bind) does not shadow it.
	out = append(out, sandbox.Mount{
		Source:   effectiveDir,
		Target:   effectiveDir,
		Type:     sandbox.MountBind,
		ReadOnly: !writable,
	})

	// 4) workspace peers (read-only).
	peerKeys := make([]string, 0, len(peers))
	for k := range peers {
		peerKeys = append(peerKeys, k)
	}
	sort.Strings(peerKeys)
	for _, k := range peerKeys {
		out = append(out, sandbox.Mount{
			Source:   peers[k],
			Target:   peers[k],
			Type:     sandbox.MountBind,
			ReadOnly: true,
		})
	}

	// 5) .boid bind-mount from the live project dir, so kit hooks / skills
	// are visible even though the tmpfs HOME above shadows the rest of the
	// host tree. Writable when the task is writable so agents can edit
	// project.yaml etc.; read-only otherwise so the hooks/skills an agent
	// runs under cannot be tampered with.
	boidSource := origProjectDir + "/.boid"
	if boidSource != "/.boid" { // ignore when origProjectDir is empty
		out = append(out, sandbox.Mount{
			Source:   boidSource,
			Target:   effectiveDir + "/.boid",
			Type:     sandbox.MountBind,
			ReadOnly: !writable,
			Guard:    dirGuardExpr(boidSource),
		})
	}

	// 6) .git ro re-bind: prevents .git/config, .git/hooks/*, etc. from being
	// modified directly inside the sandbox. The broker runs in a separate mount
	// namespace and is unaffected, so broker-mediated git operations continue to
	// work. DetectType handles both the directory case and the file case
	// (a gitdir pointer file).
	// Only needed when the effective dir is writable; read-only mounts already
	// protect .git.
	if writable {
		gitEntry := effectiveDir + "/.git"
		out = append(out, sandbox.Mount{
			Source:     gitEntry,
			Target:     gitEntry,
			Type:       sandbox.MountBind,
			ReadOnly:   true,
			DetectType: true,
			Guard:      existsGuardExpr(gitEntry),
		})
	}

	return out
}

// adapterBindingsToOrchestrator converts the adapter-facing BindMount DTO
// into the orchestrator-facing one so adapter Bindings() flow through the
// same additionalBindingMounts / buildPATH pipeline that kit-declared
// bindings do. The two structs are intentionally shape-compatible (see
// adapters.BindMount); this is a layering-only translation.
func adapterBindingsToOrchestrator(in []adapters.BindMount) []orchestrator.BindMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]orchestrator.BindMount, len(in))
	for i, bm := range in {
		out[i] = orchestrator.BindMount{
			Source:   bm.Source,
			Target:   bm.Target,
			Mode:     bm.Mode,
			IsFile:   bm.IsFile,
			Optional: bm.Optional,
		}
	}
	return out
}

func additionalBindingMounts(bindings []orchestrator.BindMount) []sandbox.Mount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]sandbox.Mount, 0, len(bindings))
	for _, bm := range bindings {
		explicitTarget := bm.Target != ""
		target := bm.Target
		if !explicitTarget {
			target = bm.Source
		}
		// Target が明示され、展開後 source と等値になった **読み取り専用** binding は
		// skip する。 worktree=false の task で ${PROJECT_WORKDIR}/x → ${WORKTREE}/x
		// が同じパスに潰れるケースで、 既に projectVisibilityMounts が見せているパスへの
		// 冗長な self-mount を避けるため。
		// 書き込み可能 (Mode=="rw") な binding は skip しない。 ProfileInit のような
		// 「ホスト root を ro-rbind した上でサブディレクトリを rw で上書きする」
		// ユースケースで Source==Target になることがあり、 そこでは rw マウントが
		// 必要なため、 Mode でガードする。
		if explicitTarget && filepath.Clean(bm.Source) == filepath.Clean(target) && bm.Mode != "rw" {
			continue
		}
		m := sandbox.Mount{
			Source:     bm.Source,
			Target:     target,
			Type:       sandbox.MountBind,
			ReadOnly:   bm.Mode != "rw",
			IsFile:     bm.IsFile,
			DetectType: !bm.IsFile,
		}
		if bm.Optional {
			// IsFile bindings need an `-e` test (file or symlink), the dir
			// case wants `-d` so an accidental file collision still fails
			// loudly. Mirrors the Phase 3-b claude binding behaviour for
			// ~/.claude.json (existsGuardExpr) vs ~/.claude (dirGuardExpr).
			if bm.IsFile {
				m.Guard = existsGuardExpr(bm.Source)
			} else {
				m.Guard = dirGuardExpr(bm.Source)
			}
		}
		out = append(out, m)
	}
	return out
}

// expandWorktreeBindings は ${WORKTREE} と ${PROJECT_WORKDIR} を per-job 値で
// 展開する。 spec_loader 側の interpolateBindMounts はこの 2 トークンを literal
// で残すので、 ここで初めて値が埋まる。 他の env 変数は meta load 時に展開済み。
func expandWorktreeBindings(bindings []orchestrator.BindMount, worktree, projectWorkDir string) []orchestrator.BindMount {
	if len(bindings) == 0 {
		return bindings
	}
	expand := func(s string) string {
		if s == "" || !strings.Contains(s, "${") {
			return s
		}
		return os.Expand(s, func(name string) string {
			switch name {
			case "WORKTREE":
				return worktree
			case "PROJECT_WORKDIR":
				return projectWorkDir
			}
			// それ以外は spec_loader で処理済み。 万一残っていたら literal を維持
			// して binding ミスを debug できるようにする。
			return "${" + name + "}"
		})
	}
	out := make([]orchestrator.BindMount, len(bindings))
	for i, bm := range bindings {
		out[i] = bm
		out[i].Source = expand(bm.Source)
		out[i].Target = expand(bm.Target)
	}
	return out
}

// sandboxShimBinDir is the fixed sandbox-internal directory where the boid
// multi-call binary and one per-host-command symlink pointing at it are
// materialized. Consumed together:
//
//   - BuildSandboxSpec bind-mounts the host boid binary at
//     `<sandboxShimBinDir>/boid`;
//   - hostCommandSymlinks materializes `<sandboxShimBinDir>/<name> -> boid`
//     for every declared host_commands entry;
//   - buildPATH prepends sandboxShimBinDir so `<name>` resolves without a
//     full path.
//
// Why `/run/boid/...` and not `/opt/boid/...`: `/opt` is in the base rbind
// list (see BuildPlan) so a spec mount at `/opt/boid/bin/boid` lands
// *inside* the host `/opt` bind mount. On the typical Linux host where
// `/opt` is root:root 755, applyMount's MkdirAll fails EACCES and every
// sandbox dispatch aborts; on the rare host where `/opt` happens to be
// user-writable, the same MkdirAll and the runner's symlink loop instead
// modify the host filesystem. `/run` is not in the base rbind list, so its
// subtree is on the sandbox's fresh tmpfs root — writable, isolated, and
// consistent with the existing `/run/boid/broker.sock` /
// `/run/boid/server.sock` / `/run/boid/docker-proxy.sock` convention.
const sandboxShimBinDir = "/run/boid/bin"

// hostCommandSymlinks materializes one `<sandboxShimBinDir>/<name> -> boid`
// symlink per declared host command name. LinkTarget is the relative
// "boid" — the symlink is created inside the same directory as the boid
// binary bind mount, so a relative target survives any future move of
// sandboxShimBinDir without a second edit.
//
// Sourced from the short-name-keyed byName view so a host_commands.<name>.path
// alias (e.g. `run-e2e: path: e2e/run.sh`) resolves as the declared name
// "run-e2e" here, not the source file's basename "run.sh".
//
// Names are validated as safe single-segment basenames before being
// concatenated into LinkPath: a host_commands map key of "../etc/passwd"
// (project.yaml is user-authored — the trust boundary is loose) would
// otherwise let a rogue project's dispatch place a symlink outside
// sandboxShimBinDir, potentially on the persistent workspace home volume,
// which would then be dereferenced/replaced by later dispatches. Invalid
// names are dropped with a warn log (defense-in-depth: they should already
// have been rejected upstream by the project spec loader; this is the last
// place the invariant can be enforced before the symlink hits the runner).
// sortedKeys keeps the output order deterministic for tests.
func hostCommandSymlinks(byName map[string]orchestrator.CommandDef) []sandbox.Symlink {
	if len(byName) == 0 {
		return nil
	}
	out := make([]sandbox.Symlink, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		if !isSafeShimName(name) {
			slog.Warn("host command shim: dropped invalid name (not a single-segment basename); would have escaped sandboxShimBinDir",
				"name", name, "shim_bin_dir", sandboxShimBinDir)
			continue
		}
		out = append(out, sandbox.Symlink{
			LinkPath:   sandboxShimBinDir + "/" + name,
			LinkTarget: "boid",
		})
	}
	return out
}

// isSafeShimName reports whether name is a usable single-segment basename
// for a symlink under sandboxShimBinDir. Rejects empty / "." / ".." / any
// name containing a path separator or NUL, and any name that would resolve
// to a path outside `sandboxShimBinDir` under Clean. Mirrors
// isSafeCloneDirName above — same trust boundary, same defense-in-depth
// posture.
func isSafeShimName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\x00") {
		return false
	}
	if strings.HasPrefix(name, "..") {
		return false
	}
	return true
}

// buildHostCommandRulesEnv builds the compact JSON payload for
// sandbox.HostCommandRulesEnv from the dispatcher's resolved, short-name-keyed
// host command defs (ResolveHostCommands' byName view — the command name the
// shim sees via CommandFromArgv0). Only commands that declare at least one
// reject rule are included; when none do, an empty string is returned so the
// caller skips setting the env var entirely. json.Marshal of a map produces
// lexicographically sorted keys, so output is deterministic.
func buildHostCommandRulesEnv(hostCommands map[string]orchestrator.CommandDef) string {
	if len(hostCommands) == 0 {
		return ""
	}
	rules := map[string][]sandbox.RejectRule{}
	for _, def := range hostCommands {
		if len(def.RejectRules) == 0 {
			continue
		}
		converted := make([]sandbox.RejectRule, len(def.RejectRules))
		for i, r := range def.RejectRules {
			converted[i] = sandbox.RejectRule{Match: r.Match, Reason: r.Reason}
		}
		rules[def.Name] = converted
	}
	if len(rules) == 0 {
		return ""
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// buildPATH prepends the workspace home's ~/.local/bin, sandboxShimBinDir
// and any additional-binding bin directories to the canonical PATH.
//
// The workspace home's ~/.local/bin comes first, ahead of everything else:
// claude/codex/opencode Bindings() all return nil, so the harness CLI a
// workspace's init.sh installs under $HOME/.local/bin (the sandbox-internal
// $HOME — see hostHomeDir()) is the only place agent binaries are expected
// to live. Giving it top PATH priority lets a workspace override a
// same-named tool it also happens to see via a legacy additional_binding or
// host_command.
//
// sandboxShimBinDir is second so any shim (`boid`, `gh`, `docker`, ... —
// see hostCommandSymlinks) resolves by short name without a full path: it
// collapses per-command PATH entries into a single entry that survives
// shim relocation.
//
// Directories already covered by the base PATH (/usr/local/bin, /usr/bin,
// /bin) are skipped, and each directory is added at most once.
func buildPATH(bindings []orchestrator.BindMount) string {
	var prefix []string
	seen := map[string]bool{}
	add := func(dir string) {
		switch dir {
		case "", "/usr/local/bin", "/usr/bin", "/bin":
			// empty or already covered by the base PATH — skip
			return
		}
		if seen[dir] {
			return
		}
		seen[dir] = true
		prefix = append(prefix, dir)
	}
	add(hostHomeDir() + "/.local/bin")
	add(sandboxShimBinDir)
	for _, bm := range bindings {
		if strings.HasSuffix(bm.Source, "/bin") {
			add(bm.Source)
		} else {
			add(bm.Source + "/bin")
		}
	}
	base := "/usr/local/bin:/usr/bin:/bin"
	if len(prefix) > 0 {
		return strings.Join(prefix, ":") + ":" + base
	}
	return base
}

// hostGatewayIP は pasta が NS に提示するゲートウェイ IP。NS 内から届くパケット
// はホストの 127.0.0.1 にマッピングされるため、これがホスト localhost への入口
// として機能する。sandbox 側 (pasta/nftables) と値を揃える。
const hostGatewayIP = "10.0.2.2"

// composeEgressServiceName is the compose
// network DNS name a container-backend job's HTTP_PROXY/HTTPS_PROXY env
// should point at instead of hostGatewayIP — see SandboxRuntimeInfo.
// ProxyHost's own doc comment. Matches internal/server's identically-named
// composeEgressServiceName constant (that package cannot import this one
// without an import cycle — internal/server already imports
// internal/dispatcher — so both sides simply agree on the same literal, the
// same way composeGatewayServiceName/gitgateway.SandboxURLOptions.
// ServiceName's own default already do).
const composeEgressServiceName = "boid-egress"

// dockerProxySandboxSocket is the fixed Unix socket path inside the sandbox
// that the per-sandbox docker proxy is bind-mounted to.
const dockerProxySandboxSocket = "/run/boid/docker-proxy.sock"

// applyProxyEnv sets the HTTP(S) proxy env vars a sandbox's outbound
// traffic is routed through.
//
// host is a parameter rather than always hostGatewayIP ("10.0.2.2", the
// pasta/slirp userns gateway IP): a docker sibling container has no such
// address at all — 10.0.2.2 is a userns/pasta artifact this backend's own
// sandbox network setup never creates — so a container-backend job that
// received this literal would have HTTP_PROXY pointing at an address
// nothing on its network ever answers, and every egress request would
// simply time out. host empty falls back to hostGatewayIP unchanged — see
// SandboxRuntimeInfo.ProxyHost's own doc comment for who sets a non-empty
// value and when.
//
// extraNoProxyHosts are additional no_proxy/NO_PROXY
// entries appended alongside host itself — either a bare hostname (no scheme,
// no port; see gatewayHostFromURL's own doc comment for why the git gateway's
// own host must always be one of them) or a CIDR block
// (SandboxRuntimeInfo.WorkspaceNetworkCIDRs, so a sibling container dialed by
// container IP bypasses the egress proxy). Empty entries are skipped so a
// caller can pass a possibly-empty gatewayHostFromURL result unconditionally.
// Duplicates are dropped: the same value reaching here twice (a gateway host
// that equals host, a subnet already listed) would otherwise show up twice in
// an env var several clients parse strictly.
func applyProxyEnv(env map[string]string, host string, port int, extraNoProxyHosts ...string) {
	if host == "" {
		host = hostGatewayIP
	}
	proxyURL := fmt.Sprintf("http://%s:%d", host, port)
	env["http_proxy"] = proxyURL
	env["https_proxy"] = proxyURL
	env["HTTP_PROXY"] = proxyURL
	env["HTTPS_PROXY"] = proxyURL
	entries := []string{host, "10.0.2.3", "localhost", "127.0.0.1"}
	seen := make(map[string]bool, len(entries)+len(extraNoProxyHosts))
	for _, e := range entries {
		seen[e] = true
	}
	for _, extra := range extraNoProxyHosts {
		if extra == "" || seen[extra] {
			continue
		}
		seen[extra] = true
		entries = append(entries, extra)
	}
	noProxy := strings.Join(entries, ",")
	env["no_proxy"] = noProxy
	env["NO_PROXY"] = noProxy
}

// gatewayHostFromURL extracts the bare hostname (no scheme, no port) from a
// git gateway base URL (rt.GatewayURL — e.g. "https://boid-gateway:39901")
// for applyProxyEnv's extraNoProxyHosts.
//
// The git gateway must always be reached directly, never through the
// egress proxy — it is boid-internal infrastructure (the sandbox-internal
// clone sequence's own upstream), not a destination any workspace's
// allowed_domains list would ever name, so the egress proxy's own
// CONNECT/domain-allowlist check (internal/sandbox/proxy.go's
// isDomainAllowed) correctly rejects it with 403 if traffic to it ever
// reaches that proxy at all: the gateway ("boid-gateway") and the egress
// proxy ("boid-egress", composeEgressServiceName) are two distinct compose
// service DNS names, so without this a clone-visibility job's `git clone`
// against the gateway would be silently routed through HTTPS_PROXY like
// any other outbound request and rejected ("CONNECT tunnel failed,
// response 403").
func gatewayHostFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// applyDockerProxyEnv injects DOCKER_HOST, CONTAINER_HOST,
// TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE, and TESTCONTAINERS_RYUK_DISABLED into
// the sandbox environment so Docker API clients and TestContainers route through
// the per-sandbox proxy socket rather than the host docker socket.
func applyDockerProxyEnv(env map[string]string) {
	sockURI := "unix://" + dockerProxySandboxSocket
	env["DOCKER_HOST"] = sockURI
	env["CONTAINER_HOST"] = sockURI
	env["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"] = dockerProxySandboxSocket
	env["TESTCONTAINERS_RYUK_DISABLED"] = "true"
}

// PeerAdvertise is the {name, clone URL, reference path} view of a workspace
// peer project. Built by Runner.buildPeerAdvertise from the peer's
// captured upstream_url + this job's gateway token; it intentionally carries
// no host filesystem path — clone-mode jobs have no host path visible for a
// peer project any more, only the sandbox-internal RO reference dir
// (ReferencePath) and the gateway clone URL an agent would `git clone` from
// if it wants to see the peer's working tree.
//
// Exposed to the agent via `boid project list`'s clone_url/reference_path/
// clone_dir fields, carried through JobContextSnapshot.WorkspacePeerAdvertise
// (job_context.go), tracked by
// Runner.Dispatch and read by BoidOpProjectList
// (internal/server/boid_executor.go) — NOT through
// SandboxRuntimeInfo.WorkspacePeerAdvertise, which stays unused; see that
// field's own doc comment.
type PeerAdvertise struct {
	// Name is the peer's repo name (the last segment of its upstream_url's
	// host/owner/repo form), used purely for display/discoverability.
	Name string
	// CloneURL is the full gateway clone URL for this peer, scoped fetch-only
	// to this job's gateway token (docs/plans/container-based-boid.md
	// 「workspace peer プロジェクト」: peers are fetch-only; writing to a peer
	// means a cross-project child task instead).
	CloneURL string
	// ReferencePath is the sandbox-internal RO bind-mount path of the peer's
	// `.git` (sandboxClonePeerReferenceDirFmt), usable as `git clone
	// --reference` when an agent does clone the peer.
	ReferencePath string
	// CloneDir is the suggested absolute sandbox-internal directory for this
	// peer, e.g. "/workspace/bm-next-lp". It is only a suggestion — nothing enforces
	// an agent actually clones the peer here — but using the same leaf name
	// projectDirName would resolve for the peer's own project (were it
	// dispatching as self) keeps the directory name stable regardless of
	// which project happens to be the one dispatching, and keeps it off
	// $HOME/tmp (both tmpfs, RAM-backed).
	CloneDir string
}

// convertHostCommands flattens the map form into a sorted slice so the
// `boid task env` RPC response (BuildWorkspaceEnvView, workspace_env_view.go
// — its sole caller) is deterministic. Kept in this file rather than moved
// to workspace_env_view.go — it has no dependency on anything else in
// sandbox_builder.go, but moving it is not required for correctness.
func convertHostCommands(commands map[string]orchestrator.CommandDef) []WorkspaceEnvHostCommand {
	if len(commands) == 0 {
		return nil
	}
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]WorkspaceEnvHostCommand, 0, len(names))
	for _, name := range names {
		def := commands[name]
		allow := append([]string(nil), def.AllowedSubcommands...)
		allow = append(allow, def.AllowedPatterns...)
		sort.Strings(allow)
		deny := append([]string(nil), def.DeniedPatterns...)
		sort.Strings(deny)
		var reject []WorkspaceEnvRejectRule
		for _, r := range def.RejectRules {
			reject = append(reject, WorkspaceEnvRejectRule{Match: r.Match, Reason: r.Reason})
		}
		out = append(out, WorkspaceEnvHostCommand{
			Name:   name,
			Allow:  allow,
			Deny:   deny,
			Reject: reject,
		})
	}
	return out
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func setIfNonEmpty(env map[string]string, key, value string) {
	if value != "" {
		env[key] = value
	}
}

func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dirGuardExpr(dir string) string {
	return "-d " + shellQuoteDir(dir)
}

func existsGuardExpr(path string) string {
	return "-e " + shellQuoteDir(path)
}

func shellQuoteDir(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-"
	for _, r := range s {
		if !strings.ContainsRune(safe, r) {
			return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
		}
	}
	return s
}

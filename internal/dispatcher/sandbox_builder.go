package dispatcher

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/registry"
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
	// host command defs produced by ResolveHostCommands
	// (docs/plans/phase5-shim-and-task-context.md, "5a: shim 固定ディレクトリ化"
	// PR1). It is the single source of truth for host command wiring:
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
	// The pre-5a-3 absolute-path-keyed sibling (ResolvedHostCommands / byPath)
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
	// to the agent via the `boid task env` broker RPC (Phase 5b PR1) so it
	// knows which hosts are reachable without burning a turn on a 403.
	AllowedDomains []string

	// GatewayURL is the git gateway's sandbox-facing base URL
	// (http://10.0.2.2:<port>), set by Runner from the daemon's own
	// gateway listener (docs/plans/git-gateway-cutover.md PR4: gateway
	// lifecycle + dispatch wiring). Empty when the gateway isn't wired.
	//
	// PR4 is inert: BuildSandboxSpec does not thread this into env or mounts
	// yet — nothing inside the sandbox reads it. The env var advertise
	// (e.g. GIT_HTTP_GATEWAY_URL) is explicitly deferred to the cutover PR
	// (PR6); the runner clone sequence that would consume it is PR5.
	GatewayURL string

	// GatewayJobToken is this job's git gateway token, registered against
	// the gateway's Registry at dispatch time (self project fetch/fetch+push,
	// workspace peers and workspace extra_repos fetch-only) and unregistered
	// when the job completes (see Runner.registerGatewayToken /
	// Runner.UnregisterJob). Empty when the gateway isn't wired.
	//
	// Same PR4-is-inert caveat as GatewayURL: carried here for PR5/PR6 to
	// consume, not yet used by BuildSandboxSpec.
	GatewayJobToken string

	// GatewayCloneURL is the full gateway clone URL for spec's own project
	// (GatewayURL + "/j/" + GatewayJobToken + "/<host>/<owner>/<repo>.git"),
	// built by Runner.buildGatewayCloneURL (docs/plans/git-gateway-cutover.md
	// PR5). Empty unless spec.Visibility.Clone is non-nil (the opt-in
	// sandbox-clone path) — computing it is otherwise wasted work, since
	// nothing would consume it. BuildSandboxSpec only reads this when
	// spec.Visibility.Clone != nil.
	GatewayCloneURL string

	// WorkspacePeerAdvertise is the {name, clone URL, reference path} view of
	// WorkspacePeers, built by Runner.buildPeerAdvertise and keyed by peer
	// project ID (docs/plans/git-gateway-cutover.md PR6 cutover 「5. peer
	// advertise の変更」 — replaces the pre-cutover host path enumeration);
	// nil when the gateway isn't wired or no peer has a resolvable
	// upstream_url.
	//
	// Currently unused by BuildSandboxSpec: environment.yaml's
	// `workspace_projects` section (its sole consumer) was removed by the
	// environment.yaml 縮退 (docs/plans/phase5-shim-and-task-context.md 決定
	// 事項 4, Phase 5b PR5) — peer advertise has no CLI replacement yet,
	// tracked as a later 5b item / separate phase. Kept here — the same
	// "carried but inert across a PR boundary" pattern as GatewayURL /
	// GatewayJobToken above — as the ready-made input for that future
	// `boid workspace peers`-style RPC.
	WorkspacePeerAdvertise map[string]PeerAdvertise

	// CloneWorkspaceDir is the host-side runtime dir path
	// (`<RuntimesDir>/<runtime_id>/workspace`) that BuildSandboxSpec bind-
	// mounts at the sandbox-internal clone target (/workspace/<name>) when
	// spec.Visibility.Clone is set (docs/plans/git-gateway-cutover.md PR6
	// cutover — 「一時領域の実体はホスト側 runtime dir の bind mount を既定と
	// する」, 2026-07-08 decision in container-based-boid.md). Allocated and
	// mkdir'd by Runner.Dispatch before BuildSandboxSpec runs, the same way
	// startDockerProxy pre-creates its runtime dir. Empty when RuntimesDir is
	// unset (e.g. minimal test wiring) — cloneMounts then skips the bind and
	// the clone lands on the sandbox's own tmpfs root instead, a safe but
	// non-default degrade (working tree + build artifacts in RAM).
	CloneWorkspaceDir string

	// WorkspaceHomeDir is the host-side per-workspace home directory
	// resolved by Runner.resolveWorkspaceHome
	// (docs/plans/home-workspace-volume.md Phase 4 PR1):
	// ~/.local/share/boid/homes/<slug>, guaranteed to exist (and, if the
	// workspace declares an init.sh, already initialized) by the time
	// Dispatch reaches BuildSandboxSpec.
	//
	// PR2 (docs/plans/home-workspace-volume.md) reads this field: the
	// Clone / projectVisible / default HOME branches below (via homeMounts)
	// bind it read-write at HOME's sandbox-internal path instead of a plain
	// tmpfs. From Phase 4 PR2 through Phase 6 PR7, $HOME/.boid was
	// additionally layered with a job-scoped tmpfs overlay to isolate
	// $HOME/.boid/output/payload_patch.json between concurrent jobs sharing
	// the same workspace home; Phase 6 PR8 (docs/plans/
	// phase6-container-backend.md §決定 9) removed that overlay once the
	// payload-patch RPC (`boid task update --payload-patch`) became the sole
	// delivery path — see homeMounts' doc comment. env["HOME"] itself is
	// unchanged — it still comes from hostHomeDir(), the *target* path
	// inside the sandbox; only the *contents* now come from the
	// workspace home instead of starting empty every job.
	//
	// When empty (test wiring that never resolved a workspace — most of
	// sandbox_builder_test.go's minimal SandboxRuntimeInfo{} literals —
	// or any other caller that has not threaded a workspace home through
	// yet) the HOME branches gracefully degrade to the pre-PR2 behaviour: a
	// single fresh tmpfs. The ProfileInit branch never reads this field at
	// all — see its own doc comment for why bind-mounting HOME there would
	// defeat its host-tool-discovery purpose.
	WorkspaceHomeDir string

	// WorkspaceSlug is the normalized workspace slug WorkspaceHomeDir was
	// resolved for (docs/plans/home-workspace-volume.md Phase 4 PR3) —
	// filepath.Base(WorkspaceHomeDir), computed once by Runner.Dispatch.
	// BuildSandboxSpec threads it into env["BOID_WORKSPACE_SLUG"] so the
	// claude/codex/opencode adapters' fail-fast "harness CLI not found"
	// error (run.go, triggered when PR3's retired adapter bindings leave no
	// CLI on PATH) can name the exact workspace whose init.sh needs the
	// install step. Empty for test wiring that never resolved a workspace
	// (most of sandbox_builder_test.go's minimal SandboxRuntimeInfo{}
	// literals) — the env var is simply omitted in that case.
	WorkspaceSlug string

	// ContainerImage is the workspace's Phase 6 container image override
	// (`orchestrator.WorkspaceMeta.ContainerImage`, docs/plans/
	// phase6-container-backend.md §決定 2/11), resolved by
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
		// (Phase 5b PR4; the file-based environment.yaml `readonly` this used
		// to read was retired by 5b-4/5b-5). The env var is still exported for
		// legacy run-agent.py and any consumer that wants to log / branch on
		// behavior name.
		// (Previously this exported BOID_INVOKED_TYPE = inst.Type, but that
		// carried the instruction phase — always "execution" — which the runner
		// mistook for a behavior name.)
		if spec.Task != nil {
			canonical, _ := orchestrator.CanonicalBehaviorName(spec.Task.Behavior)
			env["BOID_INVOKED_BEHAVIOR"] = canonical
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
	// hang するのを防ぐ (docs/plans/gitgateway-credential-fail-fast.md PR-C)。
	// 主対策は git-gateway 側の fail-fast (PR-B) だが、以下 2 経路の 401 でも
	// 同様に hang しないよう保険を張る:
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
	// kit-declared additional_bindings — that's the kit-free dispatch path
	// the Phase 3-c plan calls for. For unknown harnesses we fall back to
	// kit-declared bindings below.
	var harnessBindings []orchestrator.BindMount
	if a := registry.For(sandbox.HarnessType(spec.HarnessType)); a != nil {
		harnessBindings = adapterBindingsToOrchestrator(a.Bindings(homeDir))
	}
	// adapter-driven bindings は adapter が non-nil Bindings() を返したときだけ
	// 採用する。 spec.HarnessType != "" だけで分岐すると shell adapter
	// (Bindings()=nil) のとき pathBindings/mounts が空に潰れ、 kit 由来の
	// spawn.sh / additional_bindings が sandbox に bind されず hook script
	// が見えなくなる (E2E builtin-task-create が exit 143 で死亡する PR #594
	// 退行の真因)。 shell adapter は legacy kit binding 経路に乗せたい。
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
		applyProxyEnv(env, rt.ProxyPort)
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
	case spec.Visibility.Clone != nil:
		// Sandbox-clone path (docs/plans/git-gateway-cutover.md PR6 cutover,
		// PR5 Opus review note): skip projectVisibilityMounts entirely.
		// cloneMounts (below) mounts the reference `.git` dirs and the clone
		// target at the neutral /workspace path; there is no host ProjectDir
		// bind for this job at all, so binding projectDir here too would
		// double-mount the same host path at two sandbox targets for no
		// reason. HOME still gets the workspace home bind or a private tmpfs
		// fallback (docs/plans/home-workspace-volume.md Phase 4 PR2), exactly
		// like the "no project visible" case below.
		mounts = append(mounts, homeMounts(homeDir, rt.WorkspaceHomeDir)...)
	case projectDir != "":
		mounts = append(mounts, projectVisibilityMounts(
			projectDir,
			projectDir,
			homeDir,
			rt.WorkspaceHomeDir,
			spec.Visibility.Writable,
			rt.WorkspacePeers,
		)...)
	case spec.SandboxProfile == int(sandbox.ProfileInit):
		// ProfileInit (boid kit init / workspace configure): the plan rbinds the
		// entire host root read-only precisely so the scan can discover host
		// state, and most of the interesting tooling lives under HOME
		// (`~/.volta/bin/volta`, `~/.local/bin/go`, `~/.nvm/versions/...`, ...).
		// Layering a full HOME tmpfs on top would shadow exactly those paths and
		// make `which volta` / `ls ~/.volta/bin` return nothing — defeating the
		// whole point of ProfileInit. Layer a tmpfs over `<HOME>/.boid` only so
		// any script write under $HOME/.boid/* still lands on writable storage
		// without hiding the rest of HOME. ProfileInit jobs get no broker
		// socket (see Runner.Dispatch), so they were never a payload_patch.json
		// producer/consumer to begin with — this mount is unrelated to, and
		// unaffected by, the Phase 6 PR8 payload-patch-RPC retirement of the
		// homeMounts overlay above (docs/plans/phase6-container-backend.md
		// §決定 9).
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
	default:
		// No project visible: HOME gets the workspace home bind (+ .boid
		// tmpfs overlay) or a fresh tmpfs fallback, same as the Clone case
		// above (docs/plans/home-workspace-volume.md Phase 4 PR2).
		mounts = append(mounts, homeMounts(homeDir, rt.WorkspaceHomeDir)...)
	}

	// Sandbox-internal clone mounts (docs/plans/git-gateway-cutover.md PR5):
	// RO bind of the host project `.git` (for `git clone --reference`) and
	// the workspace peers' `.git` dirs, plus a real (non-shimmed) git binary
	// the runner's own clone/branch-resolution invocations use. Entirely
	// opt-in: nil unless spec.Visibility.Clone is set, so the existing
	// worktree/project mount layout above is completely unaffected.
	mounts = append(mounts, cloneMounts(spec, rt)...)

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
	// broker job-done reads the result from this stdout-capture file
	// (Phase 6 PR8 retired the payload-patch-file the runner used to prefer —
	// see resolveJobOutput's doc comment).
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

	// boid binary bind + host command shims (docs/plans/phase5-shim-and-task-
	// context.md, "5a: shim 固定ディレクトリ化" PR3 cutover).
	//
	// The git-shim PATH overlay (/usr/bin/git, /bin/git bound to the boid
	// binary) was retired in docs/plans/git-gateway-cutover.md PR6 cutover:
	// sandbox git is now always the real binary visible via the base rbind
	// of /usr — every job clones inside the sandbox rather than sharing a
	// host worktree, so there is no shared `.git` for a sandbox-side git
	// invocation to escape through and no reason to route git through the
	// broker any more. The broker-side git builtin and its "git"
	// BuiltinPolicy registration were subsequently deleted in PR8.
	//
	// 5a-3 replaces the pre-existing "bind boid at each host command's
	// absolute host path" scheme (hostCommandMounts, retired) with a fixed
	// directory (sandboxShimBinDir) that holds the boid multi-call binary
	// once and a symlink per host command name pointing at it. Every shim's
	// bind-mount basename now equals its declared short name by construction,
	// so the retired BOID_HOST_COMMAND_NAMES env-map lookup and the broker's
	// Path-scan fallback both become dead weight; both were dropped in the
	// same change. ProfileInit is excluded — its host `/` rbind already
	// exposes the boid binary at its real path and it declares no host
	// commands, so no shim is needed.
	var symlinks []sandbox.Symlink
	if rt.BoidBinary != "" && spec.SandboxProfile != int(sandbox.ProfileInit) {
		mounts = append(mounts, sandbox.Mount{
			Source:   rt.BoidBinary,
			Target:   sandboxShimBinDir + "/boid",
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
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
		// runner reads the result from the stdout-capture file (Phase 6 PR8
		// retired the payload-patch-file fallback this used to prefer — agents
		// / hook scripts now apply their payload patch immediately via the
		// broker's `boid task update --payload-patch` RPC instead).
		Foreground:     rt.Foreground,
		HarnessType:    harness,
		UserAnswer:     userAnswer,
		Profile:        sandbox.Profile(spec.SandboxProfile),
		Clone:          buildCloneSpec(spec, rt),
		ContainerImage: rt.ContainerImage,
	}
	return out, nil
}

// resolveWorkDir returns the initial cd target inside the sandbox. The
// sandbox-clone opt-in path (spec.Visibility.Clone != nil,
// docs/plans/git-gateway-cutover.md PR5) takes priority over the plain
// project-dir path since its bind mount above never exposes ProjectDir at
// all — the only filesystem the clone-mode sandbox has is the name-scoped
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

// sandbox-internal neutral paths used by the opt-in clone sequence
// (docs/plans/git-gateway-cutover.md PR5). Fixed rather than derived so the
// runner (which reads sandbox.CloneSpec back out of the JSON spec file) and
// dispatcher (which generates the matching mounts) always agree without
// having to thread the values through some other channel.
const (
	// sandboxCloneTargetDir is the neutral clone destination's *parent*
	// directory (docs/plans/git-gateway-cutover.md: 「clone 先は sandbox 内
	// の中立 path /workspace」; workspace 親化リファクタリング, nose
	// 2026-07-13 decision: every job actually clones into a per-project
	// subdirectory of this parent — sandboxCloneDir(name) — rather than
	// directly at this path. Two problems motivated the parent-dir switch:
	// (1) every project shared the exact same absolute sandbox cwd
	// ("/workspace"), so Claude Code's `~/.claude/projects/-workspace/`
	// session-log slug collided across every boid project; (2) an agent
	// dynamically cloning a workspace peer had no obvious place to put it
	// other than $HOME or /tmp (both tmpfs, RAM-backed) — /workspace/<peer>
	// is the natural spot once /workspace is a parent dir. See
	// PeerAdvertise.CloneDir for how a peer learns its own suggested
	// directory name. The self project has no equivalent advertised field
	// any more (environment.yaml's filesystem.clone_dir was removed by the
	// environment.yaml 縮退, Phase 5b PR5) — the sandbox actually `cd`s the
	// agent there, so `pwd` is the only source of truth for its own project's
	// directory now.
	sandboxCloneTargetDir = "/workspace"

	// sandboxCloneReferenceDir is where the host project's `.git` is RO
	// bind-mounted for use as `git clone --reference`.
	sandboxCloneReferenceDir = "/mnt/refs/self.git"

	// sandboxClonePeerReferenceDirFmt is the Sprintf pattern (keyed by peer
	// project ID) for RO bind-mounting workspace peers' `.git` dirs.
	// Nothing consumes these yet in PR5 — dynamic peer clone is later work
	// (docs/plans/container-based-boid.md 「workspace peer プロジェクト」) —
	// this only makes the mounts constructible, per PR5's scope.
	sandboxClonePeerReferenceDirFmt = "/mnt/refs/peers/%s.git"
)

// sandboxCloneDir returns the absolute sandbox-internal directory a project
// actually clones into: sandboxCloneTargetDir ("/workspace") plus the
// resolved leaf name, e.g. "/workspace/bm-next" (workspace 親化リファクタリ
// ング, nose 2026-07-13 decision). name is expected to come from
// projectDirName, which never returns empty for a project with a non-empty
// WorkDir — but an empty name here (Projects unwired, or some other
// resolution failure upstream) degrades gracefully to the bare parent dir
// itself, reproducing the pre-refactor flat "/workspace" layout instead of
// producing a malformed path like "/workspace/" or panicking.
//
// Defensive filter (PR #737 review): a name that is empty, ".", or contains
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

// cloneMounts returns the mounts for the opt-in sandbox-clone path
// (docs/plans/git-gateway-cutover.md PR5/PR6): the RO reference-repo binds
// (self + workspace peers) used for `git clone --reference`, plus the
// host-backed /workspace bind the clone actually lands on. Returns nil (no
// mounts) unless spec.Visibility.Clone is set, so the default dispatch
// path's mount list is completely unaffected.
//
// PR6 cutover removed the separate real-git-binary bind
// (sandboxRealGitBin/"/run/boid/real-git") that PR5 needed: that bind
// existed only to give the runner's own git invocations a way around the
// git-shim overlay (/usr/bin/git, /bin/git bound to the boid binary). PR6
// retires the shim overlay itself (see the "boid binary bind + host command
// mounts" section below), so the sandbox's own /usr/bin/git — visible via
// the base rbind of /usr — is already the real binary; performClone's bare
// "git" $PATH lookup resolves correctly with no extra mount needed.
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

	// /workspace bind (docs/plans/git-gateway-cutover.md PR6 cutover;
	// container-based-boid.md 2026-07-08 decision: "一時領域の実体はホスト側
	// runtime dir の bind mount を既定とする"). rt.CloneWorkspaceDir is a
	// fresh, job-scoped `<RuntimesDir>/<runtime_id>/workspace` directory
	// Runner.Dispatch pre-creates, so it already exists on the host before
	// the mount is applied — no DetectType/Guard needed, unlike the
	// reference-dir binds above (whose host source may legitimately be
	// absent, e.g. a project with no peers). Always read-write: under the
	// clone model readonly is enforced by the gateway (transport-RO), not
	// the local filesystem (docs/plans/git-gateway-cutover.md 「3. readonly
	// の意味論変更: FS-RO → transport-RO」). Empty CloneWorkspaceDir (e.g.
	// RuntimesDir unset in minimal test wiring) skips the bind — the clone
	// then simply lands on the sandbox's own tmpfs root, a safe non-default
	// degrade.
	if rt.CloneWorkspaceDir != "" {
		out = append(out, sandbox.Mount{
			Source: rt.CloneWorkspaceDir,
			Target: sandboxCloneDir(cloneDirNameForVisibility(spec.Visibility)),
			Type:   sandbox.MountBind,
		})
	}

	return out
}

// realGitBinPath resolves the host's real git binary path via $PATH. Kept as
// a dispatch-time diagnostic (docs/plans/git-gateway-cutover.md PR6, PR5
// Opus review note): post-cutover, git is a hard dependency for every
// sandbox-internal clone (the sandbox's own /usr/bin/git is an rbind of the
// daemon host's), so a LookPath failure here — meaning the daemon host has
// no git on PATH at all — is surfaced loudly rather than silently papered
// over, unlike PR5 where the same fallback was a harmless optimization
// (the shimmed /usr/bin/git still worked via the broker-dispatched git
// builtin regardless of this function's result). Nothing binds the
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
func buildCloneSpec(spec *orchestrator.JobSpec, rt SandboxRuntimeInfo) sandbox.CloneSpec {
	if spec == nil || spec.Visibility.Clone == nil {
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

// homeMounts returns the HOME mount(s) for a sandbox (docs/plans/home-
// workspace-volume.md Phase 4 PR2). When workspaceHomeDir is non-empty, HOME
// becomes a plain read-write bind of the workspace's persistent home
// directory. When workspaceHomeDir is empty (test wiring that never resolved
// a workspace home, or any caller that has not threaded
// SandboxRuntimeInfo.WorkspaceHomeDir through yet) this degrades gracefully
// to the pre-PR2 behaviour: a single fresh tmpfs at homeDir.
//
// Phase 5b PR6 (docs/plans/phase5-shim-and-task-context.md, decision 7)
// originally retired the $HOME/.boid overlay this function used to layer on
// top of the workspace-home bind, on the reasoning that contextFiles/the
// attachments RO bind — its only writers/readers under
// $HOME/.boid/{context,attachments} — were both gone, leaving only
// payload_patch.json to worry about, which PR6 tried to protect with a
// narrower per-dispatch RemoveFiles cleanup instead of a whole extra tmpfs
// mount. codex review on PR6 (Blocker, before merge) found that cleanup
// insufficient and worse, exploitable: two jobs sharing a workspace can run
// concurrently (not just sequentially, which is all the PR6 e2e coverage
// exercised), so a fixed, persistent path let one job's cleanup delete
// another's still-live patch, or one job's patch get merged into a
// completely different task — and, separately, a job's own RemoveFiles
// unlink / sentinel FileWrite operated through whatever ancestor components
// happened to be at $HOME/.boid on the persistent volume, which a prior job
// (malicious or merely buggy) could have replaced with a symlink, letting
// dispatch-time setup write/delete outside the intended output directory
// before the agent ever starts. The overlay restored there removed the
// shared, persistent path (and therefore the whole class of attack) instead
// of trying to harden operations against it; RemoveFiles (sandbox.Spec) went
// with it — see git history for the removed mechanism.
//
// Phase 6 PR8 (docs/plans/phase6-container-backend.md §決定 9) removes that
// overlay for good: agents / hook scripts now apply their payload patch
// immediately via the broker's `boid task update --payload-patch` RPC
// (Phase 5b PR7) instead of writing $HOME/.boid/output/payload_patch.json,
// so there is nothing left under $HOME/.boid worth isolating between
// concurrent jobs on the same workspace home. See resolveJobOutput
// (internal/sandbox/runner) and claude/run.go's sendTaskUpdatePayloadPatch
// for the writer/reader retirement this overlay's removal completes.
//
// Shared by the Clone branch, the default (no-project) branch and
// projectVisibilityMounts's HOME step below so all three switch over
// identically.
func homeMounts(homeDir, workspaceHomeDir string) []sandbox.Mount {
	if workspaceHomeDir == "" {
		return []sandbox.Mount{{
			Target: homeDir,
			Type:   sandbox.MountTmpfs,
		}}
	}
	return []sandbox.Mount{
		{
			Source: workspaceHomeDir,
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
	origProjectDir, effectiveDir, homeDir, workspaceHomeDir string,
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
	// host): workspace home bind, or a fresh tmpfs fallback when no
	// workspace home is resolved (docs/plans/home-workspace-volume.md
	// Phase 4 PR2).
	out = append(out, homeMounts(homeDir, workspaceHomeDir)...)

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
// materialized (docs/plans/phase5-shim-and-task-context.md 「目標状態 5a
// 完了時」/「PR 分割案 5a」PR3). Consumed together:
//
//   - BuildSandboxSpec bind-mounts the host boid binary at
//     `<sandboxShimBinDir>/boid`;
//   - hostCommandSymlinks materializes `<sandboxShimBinDir>/<name> -> boid`
//     for every declared host_commands entry;
//   - buildPATH prepends sandboxShimBinDir so `<name>` resolves without a
//     full path.
//
// Why `/run/boid/...` and not `/opt/boid/...` (the shape the plan doc had
// been sketching): `/opt` is in the base rbind list (see BuildPlan) so a
// spec mount at `/opt/boid/bin/boid` lands *inside* the host `/opt` bind
// mount. On the typical Linux host where `/opt` is root:root 755, applyMount's
// MkdirAll fails EACCES and every sandbox dispatch aborts; on the rare host
// where `/opt` happens to be user-writable, the same MkdirAll and the
// runner's symlink loop instead modify the host filesystem. `/run` is not
// in the base rbind list, so its subtree is on the sandbox's fresh tmpfs
// root — writable, isolated, and consistent with the existing
// `/run/boid/broker.sock` / `/run/boid/server.sock` / `/run/boid/docker-
// proxy.sock` convention. The container backend will bake this same path
// into the image, so no harness/skill contract needs to change when we
// switch backends. (The plan doc still lists `/opt/boid/bin` — Phase 5
// plan §決定 open item — as the sketched candidate; this PR settles it to
// `/run/boid/bin` based on the concrete sandbox mount constraints
// documented in the codex review of the 5a-3 first draft.)
const sandboxShimBinDir = "/run/boid/bin"

// hostCommandSymlinks materializes one `<sandboxShimBinDir>/<name> -> boid`
// symlink per declared host command name (docs/plans/phase5-shim-and-task-
// context.md 5a PR3 cutover). LinkTarget is the relative "boid" — the symlink
// is created inside the same directory as the boid binary bind mount, so a
// relative target survives any future move of sandboxShimBinDir without a
// second edit.
//
// Sourced from the short-name-keyed byName view so a host_commands.<name>.path
// alias (e.g. `run-e2e: path: e2e/run.sh`) resolves as the declared name
// "run-e2e" here, not the source file's basename "run.sh" — the pre-5a-3
// hostCommandMounts scheme keyed off the absolute host path, which made the
// bind-mount basename disagree with the declared name and forced the
// BOID_HOST_COMMAND_NAMES env-map lookup / broker Path-scan fallback that
// this cutover retires.
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
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retired every adapter-
// declared bind mount (claude/codex/opencode Bindings() all return nil now),
// so the harness CLI a workspace's init.sh installs under $HOME/.local/bin
// (the sandbox-internal $HOME — see hostHomeDir()) is the only place agent
// binaries are expected to live going forward. Giving it top PATH priority
// lets a workspace override a same-named tool it also happens to see via a
// legacy additional_binding or host_command.
//
// sandboxShimBinDir is second so any shim (`boid`, `gh`, `docker`, ... —
// see hostCommandSymlinks) resolves by short name without a full path. It
// replaces the pre-5a-3 "prepend each host command's parent host directory"
// scheme: shims are no longer bind-mounted at their host paths, so those
// per-command PATH entries have nothing to expose. The scheme collapses to
// a single entry that survives shim relocation and is identical across the
// userns and container backends.
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

// dockerProxySandboxSocket is the fixed Unix socket path inside the sandbox
// that the per-sandbox docker proxy is bind-mounted to.
const dockerProxySandboxSocket = "/run/boid/docker-proxy.sock"

func applyProxyEnv(env map[string]string, port int) {
	proxyURL := fmt.Sprintf("http://%s:%d", hostGatewayIP, port)
	env["http_proxy"] = proxyURL
	env["https_proxy"] = proxyURL
	env["HTTP_PROXY"] = proxyURL
	env["HTTPS_PROXY"] = proxyURL
	env["no_proxy"] = hostGatewayIP + ",10.0.2.3,localhost,127.0.0.1"
	env["NO_PROXY"] = hostGatewayIP + ",10.0.2.3,localhost,127.0.0.1"
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
// peer project (docs/plans/git-gateway-cutover.md PR6 cutover 「5. peer
// advertise の変更」). Built by Runner.buildPeerAdvertise from the peer's
// captured upstream_url + this job's gateway token; it intentionally carries
// no host filesystem path — clone-mode jobs have no host path visible for a
// peer project any more, only the sandbox-internal RO reference dir
// (ReferencePath) and the gateway clone URL an agent would `git clone` from
// if it wants to see the peer's working tree.
//
// Currently unexposed to the agent: this used to be advertised via
// environment.yaml's `workspace_projects` section, removed by the
// environment.yaml 縮退 (docs/plans/phase5-shim-and-task-context.md 決定事項
// 4, Phase 5b PR5) — see SandboxRuntimeInfo.WorkspacePeerAdvertise's doc
// comment for the current (inert, pending a future RPC) status.
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
	// peer, e.g. "/workspace/bm-next-lp" (workspace 親化リファクタリング,
	// nose 2026-07-13 decision). It is only a suggestion — nothing enforces
	// an agent actually clones the peer here — but using the same leaf name
	// projectDirName would resolve for the peer's own project (were it
	// dispatching as self) keeps the directory name stable regardless of
	// which project happens to be the one dispatching, and keeps it off
	// $HOME/tmp (both tmpfs, RAM-backed).
	CloneDir string
}

// convertHostCommands flattens the map form into a sorted slice so the
// `boid task env` RPC response (BuildWorkspaceEnvView, workspace_env_view.go
// — the sole remaining caller as of the Phase 5b PR6 cutover, which retired
// this function's other caller, the dispatch-time environment.yaml
// materialization) is deterministic. Kept in this file rather than moved to
// workspace_env_view.go to minimize the cutover's diff — it has no
// remaining dependency on anything else in sandbox_builder.go.
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

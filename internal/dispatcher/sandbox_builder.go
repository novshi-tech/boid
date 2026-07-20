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
	"gopkg.in/yaml.v3"
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

	// ResolvedHostCommands is the absolute-path-keyed view of spec.HostCommands
	// produced by ResolveHostCommands. The same map is registered with the
	// broker so the shim's os.Executable() lookup hits a known key. Empty when
	// the job declares no host commands.
	ResolvedHostCommands map[string]orchestrator.CommandDef

	// ProxySocketPath, when non-empty, is the host-side Unix socket path of the
	// per-sandbox docker proxy. sandbox_builder bind-mounts it into the sandbox
	// at the fixed sandbox path (see dockerProxySandboxSocket) and injects
	// DOCKER_HOST / CONTAINER_HOST / TESTCONTAINERS_* env vars.
	// Set by the runner before BuildSandboxSpec when capabilities.docker is
	// declared in project.yaml.
	ProxySocketPath string

	// AllowedDomains is the proxy egress allowlist. It is purely informational
	// inside the sandbox (the proxy itself enforces it on the host), surfaced
	// to the agent via environment.yaml so it knows which hosts are reachable
	// without burning a turn on a 403.
	AllowedDomains []string

	// AttachmentsRoot is the data-home directory under which per-task
	// attachments live (`<AttachmentsRoot>/tasks/<task_id>/attachments`). When
	// non-empty and the JobSpec has a TaskID, BuildSandboxSpec appends a
	// read-only bind to `<homeDir>/.boid/attachments` so the agent can read
	// user-attached files via its standard Read tool. The bind source is
	// allowed to be missing — the sandbox setup script handles that via the
	// Guard expression so attachments are optional per task.
	AttachmentsRoot string

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
	// WorkspacePeers exposed to the agent via environment.yaml's
	// `workspace_projects` (docs/plans/git-gateway-cutover.md PR6 cutover
	// 「5. peer advertise の変更」 — replaces the pre-cutover host path
	// enumeration). Built by Runner.buildPeerAdvertise, keyed by peer project
	// ID; nil when the gateway isn't wired or no peer has a resolvable
	// upstream_url. Distribution stays file-based (environment.yaml) for now
	// — RPC-based advertise is a later container-migration step (「タスクコン
	// テキストの伝搬」), out of scope here.
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
	// tmpfs, with $HOME/.boid layered as a job-scoped tmpfs on top so
	// context/output writes stay isolated per job even though the rest of
	// HOME now persists across jobs in the same workspace. env["HOME"]
	// itself is unchanged — it still comes from hostHomeDir(), the *target*
	// path inside the sandbox; only the *contents* now come from the
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
		// supervisor/executor mode from environment.yaml `readonly`. The env
		// var is still exported for legacy run-agent.py and any consumer that
		// wants to log / branch on behavior name.
		// (Previously this exported BOID_INVOKED_TYPE = inst.Type, but that
		// carried the instruction phase — always "execution" — which the runner
		// mistook for a behavior name.)
		if spec.Task != nil {
			canonical, _ := orchestrator.CanonicalBehaviorName(spec.Task.Behavior)
			env["BOID_INVOKED_BEHAVIOR"] = canonical
		}
		// Legacy env var consumed by hook scripts that predate the
		// $HOME/.boid/context/instructions.yaml context-file delivery.
		if encoded, err := json.Marshal([]orchestrator.RoutedInstruction{*inst}); err == nil {
			env["BOID_INSTRUCTIONS"] = string(encoded)
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
	env["PATH"] = buildPATH(pathBindings, rt.ResolvedHostCommands, rt.BoidBinary)
	if rules := buildHostCommandRulesEnv(rt.ResolvedHostCommands); rules != "" {
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
		// reason. HOME still gets the workspace home bind (+ .boid tmpfs
		// overlay) or a private tmpfs fallback (docs/plans/home-workspace-
		// volume.md Phase 4 PR2), exactly like the "no project visible"
		// case below.
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
		// context-file writes ($HOME/.boid/{context,output}/*) still land on
		// writable storage without hiding the rest of HOME.
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

	// Per-task attachments dir — clipboard-pasted screenshots / text uploaded
	// from the Web UI land in `<AttachmentsRoot>/tasks/<task_id>/attachments/`
	// and are exposed read-only inside the sandbox at `~/.boid/attachments`.
	// The bind is appended after the harness/kit branch above so every
	// adapter (claude / codex / opencode / shell) sees the same path. A dir
	// Guard makes the bind optional: tasks created before this feature, or
	// tasks where no attachment has ever been added, simply skip the mount.
	if rt.AttachmentsRoot != "" && spec.TaskID != "" {
		attachSrc := filepath.Join(rt.AttachmentsRoot, "tasks", spec.TaskID, "attachments")
		mounts = append(mounts, sandbox.Mount{
			Source:     attachSrc,
			Target:     homeDir + "/.boid/attachments",
			Type:       sandbox.MountBind,
			ReadOnly:   true,
			DetectType: true,
			Guard:      dirGuardExpr(attachSrc),
		})
	}

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

	// Context files: task.yaml / instructions.yaml / environment.yaml / payload.json.
	var selfCloneDir string
	if spec.Visibility.Clone != nil {
		selfCloneDir = sandboxCloneDir(cloneDirNameForVisibility(spec.Visibility))
	}
	envInput := EnvironmentInput{
		Visibility:             spec.Visibility,
		WorkspacePeers:         rt.WorkspacePeers,
		WorkspacePeerAdvertise: rt.WorkspacePeerAdvertise,
		BuiltinPolicies:        spec.BuiltinPolicies,
		HostCommands:           spec.HostCommands,
		ProxyPort:              rt.ProxyPort,
		HostGatewayIP:          hostGatewayIP,
		AllowedDomains:         rt.AllowedDomains,
		Kind:                   spec.Kind,
		HarnessType:            spec.HarnessType,
		DisplayName:            spec.DisplayName,
		CloneDir:               selfCloneDir,
	}
	files = append(files, contextFiles(
		homeDir,
		spec.Task,
		spec.Instruction,
		spec.PrimaryInput,
		envInput,
	)...)

	// Output dir sentinel — guarantees $HOME/.boid/output/ exists before the
	// user script runs, so scripts writing payload_patch.json never hit ENOENT.
	files = append(files, sandbox.FileWrite{
		Path: homeDir + "/.boid/output/.placeholder",
	})

	// stdin / stdout routing.
	//
	// Interactive jobs must inherit the PTY on stdin/stdout — piping PrimaryInput
	// via `printf | argv` or redirecting stdout to a capture file would break
	// isatty() detection in TUIs and force them into
	// non-interactive mode. Interactive hook agents read PrimaryInput from the
	// context file ($HOME/.boid/context/payload.json) rather than stdin, and the
	// runner's broker job-done reads the result from PayloadPatchPath, falling
	// back to this stdout-capture file when no payload patch was written.
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

	// boid binary bind + host command mounts.
	//
	// The git-shim PATH overlay (/usr/bin/git, /bin/git bound to the boid
	// binary) was retired in docs/plans/git-gateway-cutover.md PR6 cutover:
	// sandbox git is now always the real binary visible via the base rbind
	// of /usr — every job clones inside the sandbox rather than sharing a
	// host worktree, so there is no shared `.git` for a sandbox-side git
	// invocation to escape through and no reason to route git through the
	// broker any more. The broker-side git builtin and its "git"
	// BuiltinPolicy registration were subsequently deleted in PR8.
	if rt.BoidBinary != "" {
		// boid バイナリをホスト実パスのまま bind mount する。
		mounts = append(mounts, sandbox.Mount{
			Source:   rt.BoidBinary,
			Target:   rt.BoidBinary,
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
		// 各 host command の解決済み絶対パスに boid バイナリを bind mount し
		// shim 化する。解決は dispatcher 入り口 (runner / API / cmd exec) で
		// 行い rt.ResolvedHostCommands に積む。ここでは target を作るだけ。
		mounts = append(mounts, hostCommandMounts(rt.BoidBinary, rt.ResolvedHostCommands)...)
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
		ProxyPort:         rt.ProxyPort,
		Argv:              argv,
		WorkDir:           workDir,
		Env:               env,
		StdinBytes:        stdinBytes,
		StdoutCaptureFile: stdoutCapture,
		TTY:               tty,
		// Foreground jobs (boid exec) get no broker job-done; hook jobs leave it
		// false so runner-inner-child posts `boid job done` on agent exit. The
		// runner reads the result from PayloadPatchPath (falling back to the
		// stdout-capture file), reproducing the former EXIT-trap behaviour.
		Foreground:       rt.Foreground,
		PayloadPatchPath: homeDir + "/.boid/output/payload_patch.json",
		HarnessType:      harness,
		UserAnswer:       userAnswer,
		Profile:          sandbox.Profile(spec.SandboxProfile),
		Clone:            buildCloneSpec(spec, rt),
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
	// PeerAdvertise.CloneDir / environmentFilesystem.CloneDir for how peers
	// and the self project each learn their own suggested directory name.
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
// becomes a read-write bind of the workspace's persistent home directory,
// with $HOME/.boid layered as a job-scoped tmpfs on top so context/output
// writes stay isolated per job even though the rest of HOME now persists
// across jobs in the same workspace. When workspaceHomeDir is empty (test
// wiring that never resolved a workspace home, or any caller that has not
// threaded SandboxRuntimeInfo.WorkspaceHomeDir through yet) this degrades
// gracefully to the pre-PR2 behaviour: a single fresh tmpfs at homeDir.
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
		{
			Target: homeDir + "/.boid",
			Type:   sandbox.MountTmpfs,
		},
	}
}

// projectVisibilityMounts returns the canonical mount layout that lets the
// sandbox see the project and workspace peers, under a HOME mount (workspace
// home bind + .boid tmpfs overlay, or a tmpfs fallback — see homeMounts) that
// shadows host files but re-mounts the project on top.
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
	// host): workspace home bind + .boid tmpfs overlay, or a fresh tmpfs
	// fallback when no workspace home is resolved (docs/plans/home-
	// workspace-volume.md Phase 4 PR2).
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

// hostCommandMounts overlays the boid shim binary at every resolved host
// command path. The map is already keyed by absolute mount target — see
// ResolveHostCommands — so this function only constructs sandbox.Mount entries
// in stable order for deterministic test output.
func hostCommandMounts(boidBinary string, resolved map[string]orchestrator.CommandDef) []sandbox.Mount {
	if len(resolved) == 0 {
		return nil
	}
	out := make([]sandbox.Mount, 0, len(resolved))
	for _, target := range sortedKeys(resolved) {
		out = append(out, sandbox.Mount{
			Source:   boidBinary,
			Target:   target,
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
	}
	return out
}

// buildHostCommandRulesEnv builds the compact JSON payload for
// sandbox.HostCommandRulesEnv from the dispatcher's resolved (abs-path-keyed)
// host command defs, keyed instead by command name (the basename the shim
// sees via CommandFromArgv0). Only commands that declare at least one reject
// rule are included; when none do, an empty string is returned so the caller
// skips setting the env var entirely. json.Marshal of a map produces
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

// buildPATH prepends the workspace home's ~/.local/bin, additional-binding
// bin directories, host command directories and the boid binary directory to
// the canonical PATH.
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
// The boid binary directory is included so scripts inside the sandbox can call
// `boid` by name. Host command shims are bind-mounted at their resolved
// absolute host paths (see hostCommandMounts); those paths never appear on PATH
// on their own, so a command living outside a standard directory (e.g. a tool
// in ~/.local/bin added via the user's shell rc) would not resolve by name
// inside the sandbox. Prepending each shim's parent directory fixes that.
//
// Directories already covered by the base PATH (/usr/local/bin, /usr/bin, /bin)
// are skipped, and each directory is added at most once.
func buildPATH(bindings []orchestrator.BindMount, hostCommands map[string]orchestrator.CommandDef, boidBinary string) string {
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
	if boidBinary != "" {
		add(filepath.Dir(boidBinary))
	}
	for _, bm := range bindings {
		if strings.HasSuffix(bm.Source, "/bin") {
			add(bm.Source)
		} else {
			add(bm.Source + "/bin")
		}
	}
	// The map is keyed by absolute mount target — the same key hostCommandMounts
	// bind-mounts the shim at — so the parent directory is exactly where the
	// shim becomes visible. sortedKeys keeps the order deterministic for tests.
	for _, target := range sortedKeys(hostCommands) {
		add(filepath.Dir(target))
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

// contextFiles materializes business data under $HOME/.boid/context/:
//   - task.yaml          (from JobSpec.Task)
//   - instructions.yaml  (from JobSpec.Instruction)
//   - environment.yaml   (derived from Visibility + permissions)
//   - payload.json/yaml  (whenever PrimaryInput is present — agents read these
//     files to see verification findings / artifact / tasks regardless of
//     interactive mode. non-interactive hooks also receive PrimaryInput via
//     stdin so wrapper scripts (e.g. run-agent.py) can use it for session
//     resolution, but the agent process itself reads context files.)
func contextFiles(
	homeDir string,
	task *orchestrator.TaskSnapshot,
	inst *orchestrator.RoutedInstruction,
	primaryInput json.RawMessage,
	envInput EnvironmentInput,
) []sandbox.FileWrite {
	var out []sandbox.FileWrite
	contextDir := homeDir + "/.boid/context"

	if task != nil {
		out = append(out, sandbox.FileWrite{
			Path:    contextDir + "/task.yaml",
			Content: marshalTaskYAML(task),
		})
	}
	if inst != nil {
		out = append(out, sandbox.FileWrite{
			Path:    contextDir + "/instructions.yaml",
			Content: marshalInstructionsYAML([]orchestrator.RoutedInstruction{*inst}),
		})
	}
	out = append(out, sandbox.FileWrite{
		Path:    contextDir + "/environment.yaml",
		Content: buildEnvironmentYAML(envInput),
	})
	if len(primaryInput) > 0 {
		out = append(out, sandbox.FileWrite{
			Path:    contextDir + "/payload.json",
			Content: string(primaryInput),
		})
		out = append(out, sandbox.FileWrite{
			Path:    contextDir + "/payload.yaml",
			Content: jsonToYAML(string(primaryInput)),
		})
	}
	return out
}

func marshalTaskYAML(t *orchestrator.TaskSnapshot) string {
	m := map[string]string{
		"id":       t.ID,
		"title":    t.Title,
		"status":   t.Status,
		"behavior": t.Behavior,
	}
	if t.Description != "" {
		m["description"] = t.Description
	}
	out, _ := yaml.Marshal(m)
	return string(out)
}

func marshalInstructionsYAML(list []orchestrator.RoutedInstruction) string {
	out, _ := yaml.Marshal(list)
	return string(out)
}

// PeerAdvertise is the {name, clone URL, reference path} view of a workspace
// peer project exposed via environment.yaml (docs/plans/git-gateway-cutover.md
// PR6 cutover 「5. peer advertise の変更」). Built by Runner.buildPeerAdvertise
// from the peer's captured upstream_url + this job's gateway token; it
// intentionally carries no host filesystem path — clone-mode jobs have no
// host path visible for a peer project any more, only the sandbox-internal
// RO reference dir (ReferencePath) and the gateway clone URL an agent would
// `git clone` from if it wants to see the peer's working tree.
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

type workspaceProjectEntry struct {
	Name          string `yaml:"name"`
	CloneURL      string `yaml:"clone_url,omitempty"`
	ReferencePath string `yaml:"reference_path,omitempty"`
	CloneDir      string `yaml:"clone_dir,omitempty"`
}

// EnvironmentInput is the single input bundle for buildEnvironmentYAML. It is
// derived from JobSpec + dispatcher runtime facts before contextFiles is
// called. Centralising the inputs in one struct keeps the call sites in
// BuildSandboxSpec / tests stable as the YAML layout grows new fields.
type EnvironmentInput struct {
	Visibility     orchestrator.Visibility
	WorkspacePeers map[string]string
	// WorkspacePeerAdvertise, when non-nil, drives the workspace_projects
	// listing (docs/plans/git-gateway-cutover.md PR6 cutover). See
	// SandboxRuntimeInfo.WorkspacePeerAdvertise's doc comment.
	WorkspacePeerAdvertise map[string]PeerAdvertise
	BuiltinPolicies        map[string]orchestrator.BuiltinPolicy
	HostCommands           map[string]orchestrator.CommandDef

	// Network plumbing. ProxyPort=0 means no proxy (and therefore no egress
	// restriction wired by dispatcher). HostGatewayIP is the address agents
	// see for the host; combined with ProxyPort it becomes proxy_url.
	ProxyPort      int
	HostGatewayIP  string
	AllowedDomains []string

	// Job category — Kind=Session gates the `session:` block; everything
	// else inherits the same layout but without per-session metadata.
	Kind        orchestrator.JobKind
	HarnessType string
	DisplayName string

	// CloneDir is this job's own project's absolute sandbox-internal clone
	// directory, e.g. "/workspace/bm-next" (workspace 親化リファクタリング,
	// nose 2026-07-13 decision). Empty unless Visibility.Clone is set — the
	// `filesystem.project_dir` field already carries the *host* path for
	// descriptive purposes, but under clone-mode dispatch the host path is
	// not actually visible inside the sandbox at all, so agents need this
	// separate field to know where their own project's working tree
	// actually lives.
	CloneDir string
}

// environmentBindingEntry mirrors a BindMount in a yaml-friendly shape.
// Mode reflects what the sandbox actually applies: empty → "ro" (default),
// "rw" → "rw". IsFile is exposed so agents can tell a file mount from a
// directory mount without re-running stat.
type environmentBindingEntry struct {
	Source string `yaml:"source"`
	Target string `yaml:"target,omitempty"`
	Mode   string `yaml:"mode"`
	IsFile bool   `yaml:"is_file,omitempty"`
}

type environmentSandbox struct {
	Kind        string `yaml:"kind"`
	PIDIsolated bool   `yaml:"pid_isolated"`
	UIDInside   int    `yaml:"uid_inside"`
}

type environmentNetwork struct {
	Restricted     bool     `yaml:"restricted"`
	Egress         string   `yaml:"egress,omitempty"`
	ProxyURL       string   `yaml:"proxy_url,omitempty"`
	WebFetch       string   `yaml:"webfetch,omitempty"`
	AllowedDomains []string `yaml:"allowed_domains,omitempty"`
}

type environmentFilesystem struct {
	ProjectDir         string                    `yaml:"project_dir,omitempty"`
	Writable           bool                      `yaml:"writable"`
	Worktree           bool                      `yaml:"worktree"`
	AdditionalBindings []environmentBindingEntry `yaml:"additional_bindings,omitempty"`
	// CloneDir is the sandbox-internal absolute path this job's own project
	// actually cloned into (e.g. "/workspace/bm-next"), set only under
	// clone-mode dispatch (workspace 親化リファクタリング, nose 2026-07-13
	// decision). ProjectDir above stays the *host* path (still useful for
	// readonly/gating semantics); this field is the sandbox-internal
	// counterpart an agent can actually `cd` into or reference.
	CloneDir string `yaml:"clone_dir,omitempty"`
}

type environmentSession struct {
	Harness     string `yaml:"harness,omitempty"`
	DisplayName string `yaml:"display_name,omitempty"`
}

type environmentDoc struct {
	// Top-level fields kept for backward compatibility with skills that match
	// `readonly: true` / `tools:` / `network.restricted` by literal field name.
	// Removing them would break /boid-task dispatch logic.
	Readonly          bool                    `yaml:"readonly"`
	Worktree          bool                    `yaml:"worktree"`
	Network           environmentNetwork      `yaml:"network"`
	Tools             []string                `yaml:"tools,omitempty"`
	WorkspaceProjects []workspaceProjectEntry `yaml:"workspace_projects,omitempty"`

	// Enriched sections introduced for session-mode agent bootstrap (the
	// `boid agent claude` path). Task-mode agents read these too; the data
	// is purely additive.
	Sandbox    environmentSandbox    `yaml:"sandbox"`
	Filesystem environmentFilesystem `yaml:"filesystem"`
	// HostCommands reuses the WorkspaceEnvHostCommand shape the `boid task
	// env` RPC also returns (workspace_env_view.go) so the legacy YAML file
	// and the RPC response are built from a single convertHostCommands call
	// and cannot drift apart.
	HostCommands []WorkspaceEnvHostCommand `yaml:"host_commands,omitempty"`
	Session      *environmentSession       `yaml:"session,omitempty"`
	Notes        string                    `yaml:"notes,omitempty"`
}

// environmentNotes is the prose block agents read to learn the sandbox's
// non-obvious quirks. It is stable across jobs (the quirks are a property of
// the sandbox implementation, not of any single job) so we keep it as a
// package constant rather than rebuilding it per call. Use a literal block
// scalar in the YAML output so LLMs see it as one paragraph per bullet.
const environmentNotes = `- git はサンドボックス内の実バイナリで動作します（host への broker dispatch はありません）。 project はジョブ開始時に git gateway 経由で新規 clone されたコピーで、 origin は gateway を指しています。 commit した変更は push するまで他セッション・他ホストに共有されません。 readonly な job では push が gateway 側で拒否されます (fetch はできます)。
- $HOME はワークスペース単位で永続化された領域を bind mount しており、 同一ワークスペース内の複数ジョブ間で HOME 直下のファイル (例: adapter が書く config・キャッシュ等) が引き継がれます。 ただし Phase 4 移行途中の暫定例外として、 adapter bindings (claude/codex/opencode) はホスト側の ~/.claude / ~/.codex / ~/.opencode 等を workspace HOME 上に上書きで bind しており (Phase 4 PR3 で退役予定)、 これらのファイル群は暫定的にホスト側と共有です。 boid kit init / workspace configure ジョブ (ProfileInit) は例外的にホスト HOME を直接見ます (ホスト側のツール検出目的)。
- ~/.boid/context と ~/.boid/output はジョブごとに独立した tmpfs で、 ジョブ終了と共に消えます。 ~/.boid/attachments はタスクごとに read-only で bind される attachment ディレクトリで、 タスク終了までライフサイクルが継続します。
- boid fetch (URL 単発取得の boid builtin) は host へ broker dispatch され、 network.allowed_domains の許可ドメインに対してのみ動作します。 git fetch とは別経路で、 git の fetch/push はいずれも上の bullet の gateway 経由です。
- host_commands (gh 等) は host 側でリポジトリの checkout ディレクトリではなく中立ディレクトリで実行されます。 cwd から repo を推定する動作 (gh の暗黙 -R 等) には依存できません。 repo 文脈が必要なコマンドは kit 側の env 設定 (例: gh の GH_REPO) で渡されます。
- host_commands には stdin が渡りません。 stdin 経由でファイル内容や長文を渡す設計は使えません。
- host 側 gh CLI 経由のコマンドは --body-file のサンドボックスパスが見えません。 --body "$(cat <file>)" のように内容を展開して渡してください。
- host_commands の reject ルールに一致した呼び出しは "host_commands.<name>: rejected: <reason>" 形式のメッセージで拒否されます。 各コマンドの reject (下記 host_commands セクション) に記載の reason を読み、 代替手段に従ってください。
- Claude の WebFetch ツールはサンドボックス内では無効化されています。 Web ページを読む場合は /boid-web スキル経由で。
- additional_bindings の mode が "ro" のパスへの書き込みは EROFS / EACCES で失敗します。 project_dir.writable=false の場合も同様です。
`

// buildEnvironmentYAML derives the environment.yaml content purely from the
// primitives orchestrator already exposed: Visibility + BuiltinPolicies +
// proxy state. orchestrator does not need to know the exact YAML layout.
func buildEnvironmentYAML(in EnvironmentInput) string {
	proxyEnabled := in.ProxyPort > 0
	readonly := in.Visibility.ProjectDir != "" && !in.Visibility.Writable

	doc := environmentDoc{
		Readonly: readonly,
		// Worktree is permanently false as of docs/plans/git-gateway-cutover.md
		// PR8: host git worktree allocation is retired (every project-visible
		// job clones fresh inside the sandbox instead, PR6 cutover). The field
		// itself stays in the YAML schema — see environmentDoc's doc comment —
		// for skill/agent backward compatibility.
		Worktree: false,
		Tools:    builtinTools(in.BuiltinPolicies),

		Sandbox: environmentSandbox{
			Kind:        "rootless-userns",
			PIDIsolated: true,
			UIDInside:   0,
		},
		Network: environmentNetwork{
			Restricted:     proxyEnabled,
			AllowedDomains: append([]string(nil), in.AllowedDomains...),
		},
		Filesystem: environmentFilesystem{
			ProjectDir: in.Visibility.ProjectDir,
			Writable:   in.Visibility.Writable,
			// Permanently false — see doc.Worktree above.
			Worktree:           false,
			AdditionalBindings: convertBindings(in.Visibility.AdditionalBindings),
			CloneDir:           in.CloneDir,
		},
		HostCommands: convertHostCommands(in.HostCommands),
		Notes:        environmentNotes,
	}

	if proxyEnabled {
		doc.Network.Egress = "proxy-only"
		doc.Network.WebFetch = "disabled"
		if in.HostGatewayIP != "" {
			doc.Network.ProxyURL = fmt.Sprintf("http://%s:%d", in.HostGatewayIP, in.ProxyPort)
		}
	}

	// environment.yaml advertises peers only when the job actually sees its
	// own project filesystem. Gate jobs (ProjectDir=="") get neither peer
	// mounts nor peer listings, even though broker-side auth still covers
	// them via AllowedProjectIDs.
	//
	// docs/plans/git-gateway-cutover.md PR6 cutover 「5. peer advertise の
	// 変更」: the listing is {name, clone_url, reference_path}
	// (WorkspacePeerAdvertise), never a host filesystem path — clone-mode
	// jobs have no host path for a peer project to enumerate in the first
	// place. A peer with no advertise entry (gateway unwired, or the peer's
	// own upstream_url missing/unparseable) is simply omitted rather than
	// falling back to the retired host-path form.
	if in.Visibility.ProjectDir != "" {
		peerIDs := make([]string, 0, len(in.WorkspacePeerAdvertise))
		for id := range in.WorkspacePeerAdvertise {
			peerIDs = append(peerIDs, id)
		}
		sort.Strings(peerIDs)
		for _, id := range peerIDs {
			// PeerAdvertise and workspaceProjectEntry share the same field
			// shape (name/clone URL/reference path) by design — a direct
			// type conversion, not a struct literal copy.
			doc.WorkspaceProjects = append(doc.WorkspaceProjects, workspaceProjectEntry(in.WorkspacePeerAdvertise[id]))
		}
	}

	if in.Kind == orchestrator.JobKindSession {
		doc.Session = &environmentSession{
			Harness:     in.HarnessType,
			DisplayName: in.DisplayName,
		}
	}

	out, _ := yaml.Marshal(doc)
	return string(out)
}

// convertBindings turns orchestrator.BindMount records into the yaml-friendly
// shape exposed in environment.yaml. The default mode in orchestrator is "ro"
// (empty string), so we normalise to the literal "ro" for readability.
func convertBindings(bindings []orchestrator.BindMount) []environmentBindingEntry {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]environmentBindingEntry, 0, len(bindings))
	for _, b := range bindings {
		mode := b.Mode
		if mode == "" {
			mode = "ro"
		}
		out = append(out, environmentBindingEntry{
			Source: b.Source,
			Target: b.Target,
			Mode:   mode,
			IsFile: b.IsFile,
		})
	}
	return out
}

// convertHostCommands flattens the map form into a sorted slice so the YAML
// output (and the `boid task env` RPC response, which shares this
// conversion via BuildWorkspaceEnvView) is deterministic.
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

func builtinTools(policies map[string]orchestrator.BuiltinPolicy) []string {
	tools := []string{"git"}
	keys := make([]string, 0, len(policies))
	for k := range policies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "boid" || k == "git" || k == "fetch" {
			continue
		}
		tools = append(tools, k)
	}
	return tools
}

func jsonToYAML(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
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

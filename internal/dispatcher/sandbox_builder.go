package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"
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
// location, server socket path, resolved worktree directory, staging dirs.
type SandboxRuntimeInfo struct {
	JobID        string
	BoidBinary   string
	ServerSocket string
	ProxyPort    int

	BrokerSocket string
	BrokerToken  string

	// WorktreeDir is set by dispatcher when Visibility.UseWorktree is true,
	// having been resolved through its WorktreeManager. Empty otherwise.
	WorktreeDir string

	// WorkspacePeers maps peer project IDs (same workspace, excluding self) to
	// host paths. Dispatcher resolves this from its ProjectLookup so peer
	// visibility/authorization does not leak into orchestrator.JobSpec.
	WorkspacePeers map[string]string

	// StagingDir, when non-empty, is added to CleanupPaths so the sandbox
	// setup script removes it on teardown in addition to the caller-supplied
	// CleanupFunc.
	StagingDir string

	// RootDir, when non-empty, overrides the default per-sandbox ROOT.
	RootDir string

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

	// DockerEnabled, when true, indicates capabilities.docker is declared in
	// project.yaml.
	DockerEnabled bool

	// ProxySocketPath, when non-empty, is the host-side Unix socket path of the
	// per-sandbox docker proxy. sandbox_builder bind-mounts it into the sandbox
	// at the fixed sandbox path (see dockerProxySandboxSocket) and injects
	// DOCKER_HOST / CONTAINER_HOST / TESTCONTAINERS_* env vars.
	// Set by the runner before BuildSandboxSpec when DockerEnabled is true.
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
	workDir := resolveWorkDir(spec, rt)
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
	// Resolve adapter bindings once. When HarnessType identifies a known
	// adapter (claude/codex/opencode) its Bindings() take the place of the
	// kit-declared additional_bindings — that's the kit-free dispatch path
	// the Phase 3-c plan calls for. For unknown harnesses we fall back to
	// kit-declared bindings + KitRoots below.
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
	env["BOID_HOST_IP"] = hostGatewayIP
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

	// Project / worktree / workspace peers / .boid layer.
	projectDir := spec.Visibility.ProjectDir
	effectiveProject := projectDir
	if spec.Visibility.UseWorktree && rt.WorktreeDir != "" {
		effectiveProject = rt.WorktreeDir
	}
	if effectiveProject != "" {
		mounts = append(mounts, projectVisibilityMounts(
			projectDir,
			effectiveProject,
			homeDir,
			spec.Visibility.Writable,
			rt.WorkspacePeers,
			spec.Visibility.UseWorktree,
		)...)
	} else if spec.SandboxProfile == int(sandbox.ProfileInit) {
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
	} else {
		// No project visible: HOME is a fresh tmpfs.
		mounts = append(mounts, sandbox.Mount{
			Target: homeDir,
			Type:   sandbox.MountTmpfs,
		})
	}

	// Additional bindings and kit roots:
	//   * When the adapter declares Bindings() (claude/codex/opencode in
	//     Phase 3-c) those are the only source of bind-mounts for the agent
	//     — boid-kits' run-agent.sh / additional_bindings / KitRoots are
	//     ignored on this kit-free dispatch path that Phase 3-c introduced
	//     and Phase 3-e will lean on when the kit is retired entirely.
	//   * For every other job (boid exec, gate hooks, non-agent shell hooks
	//     dispatched via the shell adapter, kits that have not migrated to
	//     adapter-driven Bindings yet) the kit-declared bindings + KitRoots
	//     still apply. The dispatch is keyed on `len(harnessBindings) > 0`
	//     rather than `spec.HarnessType != ""` because shell adapter is
	//     declared (HarnessType="shell") but intentionally returns nil
	//     Bindings — we want those jobs on the legacy kit path until
	//     Phase 3-e collapses both.
	if len(harnessBindings) > 0 {
		mounts = append(mounts, additionalBindingMounts(harnessBindings)...)
		// ProfileInit (boid kit init / workspace configure) は host root を
		// ro-rbind した上で「書き込み先 (WritableDirs / PreCreateFiles) と追加 RO
		// bind (ReadOnlyBinds)」 を Visibility.AdditionalBindings 経由で渡してくる。
		// harness adapter (claude/codex/opencode) は agent CLI のための bindings
		// しか返さないので、 init 用 binding と経路が別。 両者を上乗せ mount しない
		// と agent はサンドボックスに kits dir が見えず、 ho-rbind の read-only
		// filesystem に阻まれて kit.yaml を書けない。 Target が claude.json 等と
		// 衝突することは仕様上ない (init binding の Source は ~/.local/share/boid/
		// kits ・ ~/.local/share/boid/workspaces ・ project workdir 群)。
		if spec.SandboxProfile == int(sandbox.ProfileInit) {
			mounts = append(mounts, additionalBindingMounts(expandedBindings)...)
		}
	} else {
		mounts = append(mounts, additionalBindingMounts(expandedBindings)...)
		for _, kitRoot := range spec.Visibility.KitRoots {
			mounts = append(mounts, sandbox.Mount{
				Source:   kitRoot,
				Target:   kitRoot,
				Type:     sandbox.MountBind,
				ReadOnly: true,
			})
		}
	}

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

	// Hook scripts live at projectDir/.boid/hooks/<id>.(sh|py) on the host but
	// the .boid directory is bind-mounted inside the sandbox at
	// worktreeDir/.boid (not at projectDir/.boid). Remap argv[0] when running
	// in worktree mode so the runner-inner-child can exec the script.
	if spec.Visibility.UseWorktree && rt.WorktreeDir != "" && projectDir != "" && len(argv) > 0 {
		boidInProject := projectDir + "/.boid/"
		if strings.HasPrefix(argv[0], boidInProject) {
			argv[0] = rt.WorktreeDir + "/.boid/" + argv[0][len(boidInProject):]
		}
	}

	// Context files: task.yaml / instructions.yaml / environment.yaml / payload.json.
	envInput := EnvironmentInput{
		Visibility:      spec.Visibility,
		WorkspacePeers:  rt.WorkspacePeers,
		BuiltinPolicies: spec.BuiltinPolicies,
		HostCommands:    spec.HostCommands,
		ProxyPort:       rt.ProxyPort,
		HostGatewayIP:   hostGatewayIP,
		AllowedDomains:  rt.AllowedDomains,
		Kind:            spec.Kind,
		HarnessType:     spec.HarnessType,
		DisplayName:     spec.DisplayName,
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
	var stdoutCapture string
	if !rt.Foreground && !spec.Interactive {
		stdoutCapture = "/tmp/boid-output"
	}

	// boid binary bind + host command mounts.
	if rt.BoidBinary != "" {
		// boid バイナリをホスト実パスのまま bind mount する。
		mounts = append(mounts, sandbox.Mount{
			Source:   rt.BoidBinary,
			Target:   rt.BoidBinary,
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
		// 実 git バイナリをサンドボックス FS から排除するため、boid shim で上書き。
		// base rbind (/usr) より後に適用されるのでこの mount が優先される。
		mounts = append(mounts, sandbox.Mount{
			Source:   rt.BoidBinary,
			Target:   "/usr/bin/git",
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
		// /bin/git: non-usrmerge 環境では /bin が独立ディレクトリになるため個別に上書き。
		// usrmerge (シンボリックリンク) でも /bin mount は独立した mount point なので必要。
		// Guard: /bin/git が存在しないホストではスキップ。
		mounts = append(mounts, sandbox.Mount{
			Source:   rt.BoidBinary,
			Target:   "/bin/git",
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
			Guard:    "-f /bin/git",
		})
		// 各 host command の解決済み絶対パスに boid バイナリを bind mount し
		// shim 化する。解決は dispatcher 入り口 (runner / API / cmd exec) で
		// 行い rt.ResolvedHostCommands に積む。ここでは target を作るだけ。
		mounts = append(mounts, hostCommandMounts(rt.BoidBinary, rt.ResolvedHostCommands)...)
	}

	var cleanup []string
	if rt.StagingDir != "" {
		cleanup = append(cleanup, rt.StagingDir)
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
		RootDir:          rt.RootDir,
		CleanupPaths:     cleanup,
		HarnessType:      harness,
		UserAnswer:       userAnswer,
		Profile:          sandbox.Profile(spec.SandboxProfile),
	}
	return out, nil
}

// resolveWorkDir returns the initial cd target inside the sandbox. Prefer the
// resolved worktree dir, otherwise the project dir, otherwise home.
func resolveWorkDir(spec *orchestrator.JobSpec, rt SandboxRuntimeInfo) string {
	if spec.Visibility.UseWorktree && rt.WorktreeDir != "" {
		return rt.WorktreeDir
	}
	if spec.Visibility.ProjectDir != "" {
		return spec.Visibility.ProjectDir
	}
	return hostHomeDir()
}

// projectVisibilityMounts returns the canonical mount layout that lets the
// sandbox see the project (or its worktree) and workspace peers, under a
// tmpfs HOME that shadows host files but re-mounts the project on top.
func projectVisibilityMounts(
	origProjectDir, effectiveDir, homeDir string,
	writable bool,
	peers map[string]string,
	worktree bool,
) []sandbox.Mount {
	var out []sandbox.Mount

	// 1) bind the effective dir (= project or worktree)
	out = append(out, sandbox.Mount{
		Source:   effectiveDir,
		Target:   effectiveDir,
		Type:     sandbox.MountBind,
		ReadOnly: !writable,
	})

	// 2) tmpfs HOME on top of user's home (isolates config files from host).
	out = append(out, sandbox.Mount{
		Target: homeDir,
		Type:   sandbox.MountTmpfs,
	})

	// 3) re-mount the effective dir so HOME tmpfs does not shadow it.
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

	// 5) .boid bind-mount (read-only) so agents can read kit hooks /
	// gates / skills, but cannot modify them.
	boidSource := origProjectDir + "/.boid"
	if boidSource != "/.boid" { // ignore when origProjectDir is empty
		out = append(out, sandbox.Mount{
			Source:   boidSource,
			Target:   effectiveDir + "/.boid",
			Type:     sandbox.MountBind,
			ReadOnly: true,
			Guard:    dirGuardExpr(boidSource),
		})
	}

	// 6) .git ro re-bind: prevents .git/config, .git/hooks/*, etc. from being
	// modified directly inside the sandbox. The broker runs in a separate mount
	// namespace and is unaffected, so broker-mediated git operations continue to
	// work. DetectType handles both the directory case (main worktrees) and the
	// file case (linked worktrees where .git is a gitdir pointer).
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

	// 7) worktree-mode: re-expose origProjectDir/.git read-only so the linked
	// worktree's gitdir pointer can be resolved (e.g. git status, log) while
	// preventing direct writes to the main .git/config. Always read-only since
	// the broker handles all writes outside the sandbox mount namespace.
	if worktree {
		gitDir := origProjectDir + "/.git"
		out = append(out, sandbox.Mount{
			Source:   gitDir,
			Target:   gitDir,
			Type:     sandbox.MountBind,
			ReadOnly: true,
			Guard:    dirGuardExpr(gitDir),
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

// buildPATH prepends additional-binding bin directories, host command
// directories and the boid binary directory to the canonical PATH.
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

type workspaceProjectEntry struct {
	Path string `yaml:"path"`
	Name string `yaml:"name"`
}

// EnvironmentInput is the single input bundle for buildEnvironmentYAML. It is
// derived from JobSpec + dispatcher runtime facts before contextFiles is
// called. Centralising the inputs in one struct keeps the call sites in
// BuildSandboxSpec / tests stable as the YAML layout grows new fields.
type EnvironmentInput struct {
	Visibility      orchestrator.Visibility
	WorkspacePeers  map[string]string
	BuiltinPolicies map[string]orchestrator.BuiltinPolicy
	HostCommands    map[string]orchestrator.CommandDef

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
	KitRoots           []string                  `yaml:"kit_roots,omitempty"`
}

type environmentSession struct {
	Harness     string `yaml:"harness,omitempty"`
	DisplayName string `yaml:"display_name,omitempty"`
}

type environmentHostCommand struct {
	Name  string   `yaml:"name"`
	Allow []string `yaml:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty"`
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
	Sandbox      environmentSandbox       `yaml:"sandbox"`
	Filesystem   environmentFilesystem    `yaml:"filesystem"`
	HostCommands []environmentHostCommand `yaml:"host_commands,omitempty"`
	Session      *environmentSession      `yaml:"session,omitempty"`
	Notes        string                   `yaml:"notes,omitempty"`
}

// environmentNotes is the prose block agents read to learn the sandbox's
// non-obvious quirks. It is stable across jobs (the quirks are a property of
// the sandbox implementation, not of any single job) so we keep it as a
// package constant rather than rebuilding it per call. Use a literal block
// scalar in the YAML output so LLMs see it as one paragraph per bullet.
const environmentNotes = `- 内部 git は host へ broker dispatch されます。 -C と -u フラグは弾かれます。 push の remote snapshot はトークン登録時に固定で、 後から gh repo create で足した remote は再 capture が必要です。
- 内部 fetch も host へ broker dispatch され、 network.allowed_domains の許可ドメインに対してのみ動作します。
- host 側 gh CLI 経由のコマンドは --body-file のサンドボックスパスが見えません。 --body "$(cat)" で stdin から渡してください。
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
		Worktree: in.Visibility.UseWorktree,
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
			ProjectDir:         in.Visibility.ProjectDir,
			Writable:           in.Visibility.Writable,
			Worktree:           in.Visibility.UseWorktree,
			AdditionalBindings: convertBindings(in.Visibility.AdditionalBindings),
			KitRoots:           append([]string(nil), in.Visibility.KitRoots...),
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
	if in.Visibility.ProjectDir != "" {
		peerIDs := make([]string, 0, len(in.WorkspacePeers))
		for id := range in.WorkspacePeers {
			peerIDs = append(peerIDs, id)
		}
		sort.Strings(peerIDs)
		for _, id := range peerIDs {
			dir := in.WorkspacePeers[id]
			doc.WorkspaceProjects = append(doc.WorkspaceProjects, workspaceProjectEntry{
				Path: dir,
				Name: filepath.Base(dir),
			})
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
// output is deterministic.
func convertHostCommands(commands map[string]orchestrator.CommandDef) []environmentHostCommand {
	if len(commands) == 0 {
		return nil
	}
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]environmentHostCommand, 0, len(names))
	for _, name := range names {
		def := commands[name]
		allow := append([]string(nil), def.AllowedSubcommands...)
		allow = append(allow, def.AllowedPatterns...)
		sort.Strings(allow)
		deny := append([]string(nil), def.DeniedPatterns...)
		sort.Strings(deny)
		out = append(out, environmentHostCommand{
			Name:  name,
			Allow: allow,
			Deny:  deny,
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


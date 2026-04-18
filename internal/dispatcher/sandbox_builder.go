package dispatcher

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
}

// BuildSandboxSpec turns a business-level JobSpec and dispatcher-side runtime
// facts into a primitive sandbox.Spec. It contains no role-aware switch: the
// mount set and environment are derived purely from JobSpec.Visibility,
// HostCommands, Instruction and Argv.
func BuildSandboxSpec(spec *orchestrator.JobSpec, rt SandboxRuntimeInfo) sandbox.Spec {
	if spec == nil {
		return sandbox.Spec{}
	}

	homeDir := hostHomeDir()
	workDir := resolveWorkDir(spec, rt)

	env := cloneStringMap(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	setIfNonEmpty(env, "BOID_TASK_ID", spec.TaskID)
	if inst := spec.Instruction; inst != nil {
		setIfNonEmpty(env, "BOID_MODEL", inst.Model)
		env["BOID_INVOKED_ROLE"] = inst.Role
		env["BOID_INVOKED_NAME"] = inst.Name
		env["BOID_INVOKED_TYPE"] = string(inst.Type)
		if inst.Interactive {
			env["BOID_INTERACTIVE"] = "1"
		}
	}
	if _, hasBoid := spec.BuiltinPolicies["boid"]; hasBoid {
		env["BOID_BUILTIN_SHIM"] = "1"
	}
	env["HOME"] = homeDir
	env["TERM"] = "xterm-256color"
	env["PATH"] = buildPATH(spec.Visibility.AdditionalBindings)
	if rt.ProxyPort > 0 {
		applyProxyEnv(env, rt.ProxyPort)
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
			spec.Visibility.WorkspacePeers,
			spec.Visibility.UseWorktree,
		)...)
	} else {
		// No project visible: HOME is a fresh tmpfs.
		mounts = append(mounts, sandbox.Mount{
			Target: homeDir,
			Type:   sandbox.MountTmpfs,
		})
	}

	// Additional bindings (kit CLIs, exec-provided pass-throughs).
	mounts = append(mounts, additionalBindingMounts(spec.Visibility.AdditionalBindings)...)

	// Server socket (exec jobs that need to talk to boid daemon).
	if rt.ServerSocket != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: rt.ServerSocket,
			Target: "/run/boid/server.sock",
			Type:   sandbox.MountBind,
			IsFile: true,
		})
		env["BOID_JOB_ID"] = rt.JobID
		env["BOID_SOCKET"] = "/run/boid/server.sock"
	}

	// Entry script: if Argv[0] lives outside the visible project area, bind
	// it read-only at a stable in-sandbox path so the sandbox can execute it.
	argv := append([]string(nil), spec.Argv...)
	if len(argv) > 0 {
		if inSandbox, extraMount, ok := stageArgv0(argv[0], effectiveProject); ok {
			argv[0] = inSandbox
			if extraMount != nil {
				mounts = append(mounts, *extraMount)
			}
		}
	}

	// Context files: task.yaml / instructions.yaml / environment.yaml / payload.json.
	files = append(files, contextFiles(
		homeDir,
		spec.Task,
		spec.Instruction,
		spec.PrimaryInput,
		spec.Visibility,
		spec.BuiltinPolicies,
		rt.ProxyPort > 0,
	)...)

	// Output dir sentinel — guarantees $HOME/.boid/output/ exists before the
	// user script runs, so scripts writing payload_patch.yaml never hit ENOENT.
	files = append(files, sandbox.FileWrite{
		Path: homeDir + "/.boid/output/.placeholder",
	})

	// stdin / stdout routing.
	interactive := spec.Instruction != nil && spec.Instruction.Interactive
	var stdinBytes []byte
	if len(spec.PrimaryInput) > 0 && !interactive {
		stdinBytes = append(stdinBytes, spec.PrimaryInput...)
	}
	var stdoutCapture string
	if !rt.Foreground {
		stdoutCapture = "/tmp/boid-output"
	}

	// boid binary bind + shim symlinks.
	if rt.BoidBinary != "" {
		mounts = append(mounts, sandbox.Mount{
			Source:   rt.BoidBinary,
			Target:   "/opt/boid/bin/boid",
			Type:     sandbox.MountBind,
			IsFile:   true,
			ReadOnly: true,
		})
	}
	symlinks := shimSymlinks(sortedKeys(spec.BuiltinPolicies), sortedKeys(spec.HostCommands))

	var cleanup []string
	if rt.StagingDir != "" {
		cleanup = append(cleanup, rt.StagingDir)
	}

	// TTY requirement: either an interactive instruction or a job kicked off
	// by an agent that expects a PTY. Concretely: whenever an instruction is
	// attached or a PrimaryInput is piped via stdin to a script, we allocate
	// a PTY so tools like claude get a proper terminal.
	tty := interactive || spec.Instruction != nil || len(stdinBytes) > 0

	var exitScript string
	if !rt.Foreground {
		exitScript = buildExitScript(rt.JobID, homeDir+"/.boid/output/payload_patch.yaml", stdoutCapture)
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
		ExitScript:        exitScript,
		TTY:               tty,
		RootDir:           rt.RootDir,
		CleanupPaths:      cleanup,
	}
	return out
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

	// 6) worktree-mode .git re-expose.
	if worktree {
		gitDir := origProjectDir + "/.git"
		out = append(out, sandbox.Mount{
			Source: gitDir,
			Target: gitDir,
			Type:   sandbox.MountBind,
			Guard:  dirGuardExpr(gitDir),
		})
	}

	return out
}

func additionalBindingMounts(bindings []orchestrator.BindMount) []sandbox.Mount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]sandbox.Mount, 0, len(bindings))
	for _, bm := range bindings {
		target := bm.Target
		if target == "" {
			target = bm.Source
		}
		out = append(out, sandbox.Mount{
			Source:     bm.Source,
			Target:     target,
			Type:       sandbox.MountBind,
			ReadOnly:   bm.Mode != "rw",
			IsFile:     bm.IsFile,
			DetectType: !bm.IsFile,
		})
	}
	return out
}

// stageArgv0 returns the in-sandbox path argv[0] should resolve to. If argv[0]
// is already under the visible project root (effectiveProject), the host path
// is reused as-is. If it is an absolute host path outside that root (e.g. a
// staged hook/gate script in /tmp), a dedicated bind is returned so the
// sandbox exposes it at a stable path. Bare command names are left untouched.
func stageArgv0(original, effectiveProject string) (string, *sandbox.Mount, bool) {
	if original == "" || !filepath.IsAbs(original) {
		return "", nil, false
	}
	if effectiveProject != "" && strings.HasPrefix(original, effectiveProject+string(filepath.Separator)) {
		return original, nil, false
	}
	target := "/opt/boid/entry/" + filepath.Base(original)
	return target, &sandbox.Mount{
		Source:   original,
		Target:   target,
		Type:     sandbox.MountBind,
		IsFile:   true,
		ReadOnly: true,
	}, true
}

// shimSymlinks creates /opt/boid/bin/<cmd> → boid symlinks for every command
// name used by the job (builtin or broker-provided).
func shimSymlinks(builtins, hostCommands []string) []sandbox.Symlink {
	seen := map[string]struct{}{}
	add := func(name string) []sandbox.Symlink {
		if name == "boid" {
			return nil
		}
		if _, ok := seen[name]; ok {
			return nil
		}
		seen[name] = struct{}{}
		return []sandbox.Symlink{{LinkTarget: "boid", LinkPath: "/opt/boid/bin/" + name}}
	}
	var out []sandbox.Symlink
	for _, n := range builtins {
		out = append(out, add(n)...)
	}
	for _, n := range hostCommands {
		out = append(out, add(n)...)
	}
	return out
}

// buildPATH prepends additional-binding bin directories to the canonical PATH.
func buildPATH(bindings []orchestrator.BindMount) string {
	var prefix []string
	for _, bm := range bindings {
		if strings.HasSuffix(bm.Source, "/bin") {
			prefix = append(prefix, bm.Source)
		} else {
			prefix = append(prefix, bm.Source+"/bin")
		}
	}
	base := "/opt/boid/bin:/usr/local/bin:/usr/bin:/bin"
	if len(prefix) > 0 {
		return strings.Join(prefix, ":") + ":" + base
	}
	return base
}

// buildExitScript renders the EXIT trap that calls `boid job done`.
func buildExitScript(jobID, payloadFile, stdoutFallback string) string {
	var b strings.Builder
	b.WriteString("_exit=$?\n")
	fmt.Fprintf(&b, "mkdir -p \"$(dirname %q)\"\n", payloadFile)
	fmt.Fprintf(&b, "if [ -f %q ]; then\n", payloadFile)
	fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit --output-file %q\n", jobID, payloadFile)
	if stdoutFallback != "" {
		b.WriteString("else\n")
		fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit --output-file %q\n", jobID, stdoutFallback)
	} else {
		b.WriteString("else\n")
		fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit\n", jobID)
	}
	b.WriteString("fi")
	return b.String()
}

func applyProxyEnv(env map[string]string, port int) {
	proxyURL := fmt.Sprintf("http://10.0.2.2:%d", port)
	env["http_proxy"] = proxyURL
	env["https_proxy"] = proxyURL
	env["HTTP_PROXY"] = proxyURL
	env["HTTPS_PROXY"] = proxyURL
	env["no_proxy"] = "10.0.2.2,10.0.2.3,localhost,127.0.0.1"
	env["NO_PROXY"] = "10.0.2.2,10.0.2.3,localhost,127.0.0.1"
}

// contextFiles materializes business data under $HOME/.boid/context/:
//   - task.yaml          (from JobSpec.Task)
//   - instructions.yaml  (from JobSpec.Instruction)
//   - environment.yaml   (derived from Visibility + permissions)
//   - payload.json/yaml  (only for interactive agents — PrimaryInput fed as file
//     instead of stdin)
func contextFiles(
	homeDir string,
	task *orchestrator.TaskSnapshot,
	inst *orchestrator.RoutedInstruction,
	primaryInput json.RawMessage,
	visibility orchestrator.Visibility,
	policies map[string]orchestrator.BuiltinPolicy,
	proxyEnabled bool,
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
		Content: buildEnvironmentYAML(visibility, policies, proxyEnabled),
	})
	if inst != nil && inst.Interactive && len(primaryInput) > 0 {
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

type environmentDoc struct {
	Readonly          bool                    `yaml:"readonly"`
	Worktree          bool                    `yaml:"worktree"`
	Network           map[string]bool         `yaml:"network"`
	Tools             []string                `yaml:"tools,omitempty"`
	WorkspaceProjects []workspaceProjectEntry `yaml:"workspace_projects,omitempty"`
}

// buildEnvironmentYAML derives the environment.yaml content purely from the
// primitives orchestrator already exposed: Visibility + BuiltinPolicies +
// proxy state. orchestrator does not need to know the exact YAML layout.
func buildEnvironmentYAML(visibility orchestrator.Visibility, policies map[string]orchestrator.BuiltinPolicy, proxyEnabled bool) string {
	env := environmentDoc{
		Readonly: visibility.ProjectDir != "" && !visibility.Writable,
		Worktree: visibility.UseWorktree,
		Network:  map[string]bool{"restricted": proxyEnabled},
		Tools:    builtinTools(policies),
	}
	peerIDs := make([]string, 0, len(visibility.WorkspacePeers))
	for id := range visibility.WorkspacePeers {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)
	for _, id := range peerIDs {
		dir := visibility.WorkspacePeers[id]
		env.WorkspaceProjects = append(env.WorkspaceProjects, workspaceProjectEntry{
			Path: dir,
			Name: filepath.Base(dir),
		})
	}
	out, _ := yaml.Marshal(env)
	return string(out)
}

func builtinTools(policies map[string]orchestrator.BuiltinPolicy) []string {
	tools := []string{"git"}
	keys := make([]string, 0, len(policies))
	for k := range policies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "boid" || k == "git" {
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

func shellQuoteDir(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-"
	for _, r := range s {
		if !strings.ContainsRune(safe, r) {
			return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
		}
	}
	return s
}


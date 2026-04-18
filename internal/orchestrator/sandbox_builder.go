package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
	"gopkg.in/yaml.v3"
)

// SandboxBuildOptions carries runtime-injected fields that only become known
// after dispatcher-side setup (job id assignment, broker registration, gate
// script staging, sandbox root allocation).
type SandboxBuildOptions struct {
	// JobID is assigned by the dispatcher when creating the Job DB entry.
	JobID string
	// RootDir is the sandbox ROOT directory (pre-allocated by dispatcher).
	RootDir string
	// BrokerSocket is the host-side broker UNIX socket path (if broker is enabled).
	BrokerSocket string
	// BrokerToken is the per-job broker authentication token (if broker is enabled).
	BrokerToken string
	// StagedGatesDir, when non-empty, overrides req.GatesDir (used after
	// dispatcher stages kit+project gate scripts into a temp dir for gate jobs).
	StagedGatesDir string
	// StagingDir, when non-empty, is added to sandbox.Spec.CleanupPaths so the
	// setup script's cleanup trap removes it on exit.
	StagingDir string
}

// ExecSandboxBuildInput carries the inputs needed to build a sandbox.Spec for
// a user-initiated `boid exec` invocation. Distinct from JobSpec
// because exec has no Task/Role/HookScript and provides Argv directly.
type ExecSandboxBuildInput struct {
	JobID              string
	ProjectID          string
	ProjectDir         string
	HomeDir            string
	Argv               []string
	BoidBinary         string
	ServerSocket       string
	BrokerSocket       string
	BrokerToken        string
	Env                map[string]string
	BuiltinCommands    []string
	HostCommands       []string
	AdditionalBindings []BindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	TTY                bool
	EnvironmentYAML    string
	RootDir            string
}

// BuildSandboxSpec translates a JobSpec (orchestrator vocabulary:
// Role, Task, Instruction, Hook, Gate) into a primitive-only sandbox.Spec.
// This is the role-aware seam — dispatcher consumes only sandbox.Spec and
// has no knowledge of Role or any higher-level concept.
func BuildSandboxSpec(req JobSpec, opts SandboxBuildOptions) sandbox.Spec {
	workDir := effectiveWorkDir(req)
	homeDir := effectiveHomeDir(req)

	env := cloneStringMap(req.Env)
	if env == nil {
		env = map[string]string{}
	}

	setIfNonEmpty := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	setIfNonEmpty("BOID_TASK_ID", req.TaskID)
	setIfNonEmpty("BOID_INSTRUCTIONS", req.InstructionsJSON)
	setIfNonEmpty("BOID_MODEL", req.Model)
	if req.InvokedRole != "" || req.InvokedName != "" || req.InvokedType != "" {
		env["BOID_INVOKED_ROLE"] = req.InvokedRole
		env["BOID_INVOKED_NAME"] = req.InvokedName
		env["BOID_INVOKED_TYPE"] = req.InvokedType
	}
	if _, hasBoid := req.BuiltinPolicies["boid"]; hasBoid {
		env["BOID_BUILTIN_SHIM"] = "1"
	}

	env["HOME"] = homeDir
	env["TERM"] = "xterm-256color"
	env["PATH"] = BuildPATH(req.AdditionalBindings)

	if req.ProxyPort > 0 {
		applyProxyEnv(env, req.ProxyPort)
	}

	var mounts []sandbox.Mount

	// Broker socket + token (filled by dispatcher at registration time).
	if opts.BrokerSocket != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: opts.BrokerSocket,
			Target: "/run/boid/broker.sock",
			Type:   sandbox.MountBind,
			IsFile: true,
		})
		env["BOID_BROKER_SOCKET"] = "/run/boid/broker.sock"
	}
	if opts.BrokerToken != "" {
		env["BOID_BROKER_TOKEN"] = opts.BrokerToken
	}

	// Context files materialized at $HOME/.boid/context/.
	files := contextFiles(homeDir, req.TaskYAML, req.EnvironmentYAML, req.InstructionsJSON, req.PayloadJSON)

	var argv []string
	var stdinBytes []byte
	var stdoutCapture string
	var exitScript string

	switch req.Role {
	case RoleHook:
		mounts = append(mounts, projectMountsWithHooks(req.ProjectDir, workDir, homeDir, req.WorktreeDir, req.Readonly, req.WorkspaceDirs, len(req.HookFiles) > 0)...)
		mounts = append(mounts, HookFileMounts(workDir, req.ProjectDir, req.HookFiles)...)
		mounts = append(mounts, AdditionalBindingMounts(req.AdditionalBindings)...)
		argv = resolveHookArgv(req, workDir)
		// Interactive controls payload delivery: stdin (default) vs. context file.
		// PTY allocation is a separate concern handled by the TTY computation below.
		if !req.Interactive {
			stdinBytes = []byte(req.PayloadJSON)
		} else {
			env["BOID_INTERACTIVE"] = "1"
		}
		exitScript = BuildExitScript(opts.JobID, "$HOME/.boid/output/payload_patch.yaml", "")
	case RoleGate:
		env["HOME"] = "/tmp"
		mounts = append(mounts, sandbox.Mount{Target: "/tmp", Type: sandbox.MountTmpfs})
		if workDir != "" {
			mounts = append(mounts, sandbox.Mount{Target: workDir, Type: sandbox.MountTmpfs})
		}
		gatesDir := opts.StagedGatesDir
		if gatesDir == "" {
			gatesDir = req.GatesDir
		}
		if gatesDir == "" {
			gatesDir = req.ProjectDir + "/.boid/gates"
		}
		if req.HookScript != "" {
			mounts = append(mounts, sandbox.Mount{
				Source: gatesDir + "/" + req.HookScript,
				Target: "/opt/boid/gates/" + req.HookScript,
				Type:   sandbox.MountBind,
				IsFile: true,
			})
		}
		mounts = append(mounts, AdditionalBindingMounts(req.AdditionalBindings)...)
		argv = resolveGateArgv(req)
		stdinBytes = []byte(req.TaskJSON)
		stdoutCapture = "/tmp/boid-output"
		exitScript = BuildExitScript(opts.JobID, "$HOME/.boid/output/payload_patch.yaml", "/tmp/boid-output")
	default:
		// Unknown Role: treat like a tracked command with project access.
		mounts = append(mounts, ProjectMounts(req.ProjectDir, workDir, homeDir, req.WorktreeDir, false, req.WorkspaceDirs)...)
		mounts = append(mounts, AdditionalBindingMounts(req.AdditionalBindings)...)
		if req.ServerSocket != "" {
			mounts = append(mounts, sandbox.Mount{
				Source: req.ServerSocket,
				Target: "/run/boid/server.sock",
				Type:   sandbox.MountBind,
				IsFile: true,
			})
			env["BOID_JOB_ID"] = opts.JobID
			env["BOID_SOCKET"] = "/run/boid/server.sock"
		}
	}

	mounts = append(mounts, sandbox.Mount{
		Source:   req.BoidBinary,
		Target:   "/opt/boid/bin/boid",
		Type:     sandbox.MountBind,
		IsFile:   true,
		ReadOnly: true,
	})
	symlinks := ShimSymlinks(sortedPolicyKeys(req.BuiltinPolicies), hostCommandNames(req.HostCommands))

	var cleanup []string
	if opts.StagingDir != "" {
		cleanup = append(cleanup, opts.StagingDir)
	}

	return sandbox.Spec{
		ID:                opts.JobID,
		Mounts:            mounts,
		Files:             files,
		Symlinks:          symlinks,
		ProxyPort:         req.ProxyPort,
		Argv:              argv,
		WorkDir:           workDir,
		Env:               env,
		StdinBytes:        stdinBytes,
		StdoutCaptureFile: stdoutCapture,
		ExitScript:        exitScript,
		// TTY (PTY allocation) is required when:
		//   - Interactive=true (user-facing intent for an interactive job), or
		//   - the role launches an agent that expects a PTY (Hook runs Claude;
		//     Gate runs a script reading TaskJSON via /dev/stdin under PTY).
		// The first axis is user-driven, the latter is role-derived; keeping
		// them OR'd here documents that PTY is the union, not a single concern.
		TTY:               req.Interactive || req.Role == RoleHook || req.Role == RoleGate,
		RootDir:           opts.RootDir,
		CleanupPaths:      cleanup,
	}
}

// BuildExecSandboxSpec builds a sandbox.Spec for a user-initiated `boid exec`
// invocation. The key differences from BuildSandboxSpec: Argv is provided
// directly (no HookScript), there is no Task/Role/context, and the server
// socket is mounted so the exec'd process can talk to the daemon.
func BuildExecSandboxSpec(in ExecSandboxBuildInput) sandbox.Spec {
	workDir := in.ProjectDir
	homeDir := in.HomeDir
	if homeDir == "" {
		homeDir = in.ProjectDir
	}

	env := cloneStringMap(in.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["HOME"] = homeDir
	env["TERM"] = "xterm-256color"
	env["PATH"] = BuildPATH(in.AdditionalBindings)
	setIfContains(env, "BOID_BUILTIN_SHIM", "1", in.BuiltinCommands, "boid")
	if in.ProxyPort > 0 {
		applyProxyEnv(env, in.ProxyPort)
	}

	var mounts []sandbox.Mount
	if in.BrokerSocket != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: in.BrokerSocket,
			Target: "/run/boid/broker.sock",
			Type:   sandbox.MountBind,
			IsFile: true,
		})
		env["BOID_BROKER_SOCKET"] = "/run/boid/broker.sock"
	}
	if in.BrokerToken != "" {
		env["BOID_BROKER_TOKEN"] = in.BrokerToken
	}

	mounts = append(mounts, ProjectMounts(in.ProjectDir, workDir, homeDir, "", false, in.WorkspaceDirs)...)
	mounts = append(mounts, AdditionalBindingMounts(in.AdditionalBindings)...)
	if in.ServerSocket != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: in.ServerSocket,
			Target: "/run/boid/server.sock",
			Type:   sandbox.MountBind,
			IsFile: true,
		})
		env["BOID_JOB_ID"] = in.JobID
		env["BOID_SOCKET"] = "/run/boid/server.sock"
	}

	mounts = append(mounts, sandbox.Mount{
		Source:   in.BoidBinary,
		Target:   "/opt/boid/bin/boid",
		Type:     sandbox.MountBind,
		IsFile:   true,
		ReadOnly: true,
	})

	var envFile []sandbox.FileWrite
	if in.EnvironmentYAML != "" {
		envFile = []sandbox.FileWrite{{
			Path:    homeDir + "/.boid/context/environment.yaml",
			Content: in.EnvironmentYAML,
		}}
	}

	return sandbox.Spec{
		ID:       in.JobID,
		Mounts:   mounts,
		Files:    envFile,
		Symlinks: ShimSymlinks(in.BuiltinCommands, in.HostCommands),

		ProxyPort: in.ProxyPort,
		Argv:      in.Argv,
		WorkDir:   workDir,
		Env:       env,
		TTY:       in.TTY,
		RootDir:   in.RootDir,
	}
}

// ProjectMounts returns the standard filesystem layout for a sandbox that
// sees the project: project bind → HOME tmpfs → project re-mount → peers (ro)
// → .boid (ro) → (.git remount in worktree mode).
func ProjectMounts(projectDir, workDir, homeDir, worktreeDir string, readOnly bool, workspacePeers map[string]string) []sandbox.Mount {
	return projectMountsWithHooks(projectDir, workDir, homeDir, worktreeDir, readOnly, workspacePeers, false)
}

// projectMountsWithHooks is the full form used when callers may want the
// .boid bind-mount to pre-create a "hooks" subdir (so a later tmpfs can cover
// it and individual hook files can be bind-mounted on top).
func projectMountsWithHooks(projectDir, workDir, homeDir, worktreeDir string, readOnly bool, workspacePeers map[string]string, needsHooksDir bool) []sandbox.Mount {
	var out []sandbox.Mount

	out = append(out, sandbox.Mount{
		Source:   workDir,
		Target:   workDir,
		Type:     sandbox.MountBind,
		ReadOnly: readOnly,
	})

	out = append(out, sandbox.Mount{
		Target: homeDir,
		Type:   sandbox.MountTmpfs,
	})

	out = append(out, sandbox.Mount{
		Source:   workDir,
		Target:   workDir,
		Type:     sandbox.MountBind,
		ReadOnly: readOnly,
	})

	peerKeys := sortedKeys(workspacePeers)
	for _, k := range peerKeys {
		out = append(out, sandbox.Mount{
			Source:   workspacePeers[k],
			Target:   workspacePeers[k],
			Type:     sandbox.MountBind,
			ReadOnly: true,
		})
	}

	boidSource := projectDir + "/.boid"
	boidMount := sandbox.Mount{
		Source:   boidSource,
		Target:   workDir + "/.boid",
		Type:     sandbox.MountBind,
		ReadOnly: true,
		Guard:    dirGuardExpr(boidSource),
	}
	if needsHooksDir {
		// Pre-create hooks/ inside the bind so the subsequent tmpfs target
		// exists. The mkdir runs before the read-only remount.
		boidMount.NeedsDirs = []string{"hooks"}
	}
	out = append(out, boidMount)

	if worktreeDir != "" {
		gitDir := projectDir + "/.git"
		out = append(out, sandbox.Mount{
			Source: gitDir,
			Target: gitDir,
			Type:   sandbox.MountBind,
			Guard:  dirGuardExpr(gitDir),
		})
	}

	return out
}

// HookFileMounts returns the tmpfs-over-.boid/hooks pattern: a tmpfs layer
// that allows individual hook files to be bind-mounted read-only on top of
// the otherwise read-only .boid directory.
func HookFileMounts(workDir, projectDir string, files []HookFile) []sandbox.Mount {
	if len(files) == 0 {
		return nil
	}
	boidSource := projectDir + "/.boid"
	hooksTarget := workDir + "/.boid/hooks"

	out := []sandbox.Mount{
		{Target: hooksTarget, Type: sandbox.MountTmpfs, Guard: dirGuardExpr(boidSource)},
	}
	for _, hf := range files {
		out = append(out, sandbox.Mount{
			Source:   hf.Source,
			Target:   hooksTarget + "/" + hf.TargetName,
			Type:     sandbox.MountBind,
			ReadOnly: true,
			IsFile:   true,
			Guard:    dirGuardExpr(boidSource),
		})
	}
	return out
}

// AdditionalBindingMounts converts a BindMount slice into Mount entries.
// Target defaults to Source when empty; IsFile skips runtime type detection.
func AdditionalBindingMounts(bindings []BindMount) []sandbox.Mount {
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

// ShimSymlinks creates /opt/boid/bin/<cmd> → boid symlinks for each unique
// command name. The boid binary itself is skipped because it is bind-mounted
// directly at /opt/boid/bin/boid.
func ShimSymlinks(builtins, hostCommands []string) []sandbox.Symlink {
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

// BuildPATH prepends additional-binding bin directories to the canonical PATH.
func BuildPATH(bindings []BindMount) string {
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

// BuildExitScript renders the shell snippet for the EXIT trap that calls
// `boid job done <jobID>` with --exit-code and optionally --output-file.
func BuildExitScript(jobID, payloadFile, stdoutFallback string) string {
	var b strings.Builder
	b.WriteString("_exit=$?\n")
	fmt.Fprintf(&b, "mkdir -p \"$(dirname %s)\"\n", quoteForTrap(payloadFile))
	fmt.Fprintf(&b, "if [ -f %s ]; then\n", quoteForTrap(payloadFile))
	fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit --output-file %s\n", jobID, quoteForTrap(payloadFile))
	if stdoutFallback != "" {
		b.WriteString("else\n")
		fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit --output-file %s\n", jobID, quoteForTrap(stdoutFallback))
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

func setIfContains(env map[string]string, key, value string, list []string, needle string) {
	for _, item := range list {
		if item == needle {
			env[key] = value
			return
		}
	}
}

func contextFiles(homeDir, taskYAML, envYAML, instructionsJSON, payloadJSON string) []sandbox.FileWrite {
	if taskYAML == "" && envYAML == "" && instructionsJSON == "" && payloadJSON == "" {
		return nil
	}
	contextDir := homeDir + "/.boid/context"
	var out []sandbox.FileWrite
	if taskYAML != "" {
		out = append(out, sandbox.FileWrite{Path: contextDir + "/task.yaml", Content: taskYAML})
	}
	if envYAML != "" {
		out = append(out, sandbox.FileWrite{Path: contextDir + "/environment.yaml", Content: envYAML})
	}
	if instructionsJSON != "" {
		out = append(out, sandbox.FileWrite{Path: contextDir + "/instructions.yaml", Content: jsonToYAML(instructionsJSON)})
	}
	if payloadJSON != "" {
		out = append(out, sandbox.FileWrite{Path: contextDir + "/payload.yaml", Content: jsonToYAML(payloadJSON)})
		out = append(out, sandbox.FileWrite{Path: contextDir + "/payload.json", Content: payloadJSON})
	}
	return out
}

func quoteForTrap(s string) string { return "\"" + s + "\"" }

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

func effectiveWorkDir(req JobSpec) string {
	if req.WorktreeDir != "" {
		return req.WorktreeDir
	}
	return req.ProjectDir
}

func effectiveHomeDir(req JobSpec) string {
	if req.HomeDir != "" {
		return req.HomeDir
	}
	return req.ProjectDir
}

func resolveHookArgv(req JobSpec, workDir string) []string {
	if req.HookScript == "" {
		return nil
	}
	return []string{workDir + "/.boid/hooks/" + req.HookScript}
}

func resolveGateArgv(req JobSpec) []string {
	if req.HookScript == "" {
		return nil
	}
	return []string{"/opt/boid/gates/" + req.HookScript}
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

func sortedPolicyKeys(policies map[string]BuiltinPolicy) []string {
	return sortedKeys(policies)
}

func hostCommandNames(cmds map[string]CommandDef) []string {
	if len(cmds) == 0 {
		return nil
	}
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

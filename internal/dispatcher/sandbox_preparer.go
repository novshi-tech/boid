package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
	"gopkg.in/yaml.v3"
)

type sandboxPreparerImpl struct{}

// NewSandboxPreparer returns the sandbox provider adapter for dispatcher-owned sandbox specs.
func NewSandboxPreparer() SandboxPreparer {
	return sandboxPreparerImpl{}
}

func (sandboxPreparerImpl) PrepareSandbox(spec SandboxSpec) (*PreparedSandbox, error) {
	rootDir, err := os.MkdirTemp("", "boid-root-")
	if err != nil {
		return nil, fmt.Errorf("create sandbox root: %w", err)
	}

	sbSpec := buildSandboxSpec(spec, rootDir)

	outerPath, err := sandbox.Prepare(sbSpec)
	if err != nil {
		_ = os.RemoveAll(rootDir)
		return nil, err
	}

	prefix := strings.TrimSuffix(outerPath, "-outer.sh")
	scriptPaths := []string{
		outerPath,
		prefix + "-setup.sh",
		prefix + "-inner.sh",
	}

	return &PreparedSandbox{
		OuterPath:   outerPath,
		RootDir:     rootDir,
		ScriptPaths: scriptPaths,
		StagingDir:  spec.StagingDir,
	}, nil
}

// buildSandboxSpec translates the dispatcher-layer SandboxSpec (still carrying
// Role / Broker / ServerSocket / HookScript concepts) into a primitive-only
// sandbox.Spec. All mount construction, env building, exit-script rendering,
// and role-based dispatch is centralized here. M5 will move this into
// orchestrator so dispatcher stops knowing Role altogether.
func buildSandboxSpec(spec SandboxSpec, rootDir string) sandbox.Spec {
	env := cloneStringMap(spec.Env)
	workDir := effectiveWorkDir(spec)
	homeDir := effectiveHomeDir(spec)

	// Env composition — everything that used to be a dedicated WrapperConfig
	// field now rides here as a regular env entry.
	setIfNonEmpty := func(k, v string) {
		if v == "" {
			return
		}
		if env == nil {
			env = map[string]string{}
		}
		env[k] = v
	}
	setIfNonEmpty("BOID_TASK_ID", spec.TaskID)
	setIfNonEmpty("BOID_INSTRUCTIONS", spec.InstructionsJSON)
	setIfNonEmpty("BOID_MODEL", spec.Model)
	if spec.InvokedRole != "" || spec.InvokedName != "" || spec.InvokedType != "" {
		if env == nil {
			env = map[string]string{}
		}
		env["BOID_INVOKED_ROLE"] = spec.InvokedRole
		env["BOID_INVOKED_NAME"] = spec.InvokedName
		env["BOID_INVOKED_TYPE"] = spec.InvokedType
	}
	for _, name := range sortedPolicyKeys(spec.BuiltinPolicies) {
		if name == "boid" {
			if env == nil {
				env = map[string]string{}
			}
			env["BOID_BUILTIN_SHIM"] = "1"
			break
		}
	}

	// HOME is always the primary shell expectation, TERM keeps TUI apps happy.
	if env == nil {
		env = map[string]string{}
	}
	env["HOME"] = homeDir
	env["TERM"] = "xterm-256color"
	env["PATH"] = buildPATH(spec.AdditionalBindings)

	if spec.ProxyPort > 0 {
		proxyURL := fmt.Sprintf("http://10.0.2.2:%d", spec.ProxyPort)
		env["http_proxy"] = proxyURL
		env["https_proxy"] = proxyURL
		env["HTTP_PROXY"] = proxyURL
		env["HTTPS_PROXY"] = proxyURL
		env["no_proxy"] = "10.0.2.2,10.0.2.3,localhost,127.0.0.1"
		env["NO_PROXY"] = "10.0.2.2,10.0.2.3,localhost,127.0.0.1"
	}

	// --- Mounts ---
	var mounts []sandbox.Mount

	// Broker socket + token
	if spec.BrokerSocket != "" {
		mounts = append(mounts, sandbox.Mount{
			Source: spec.BrokerSocket,
			Target: "/run/boid/broker.sock",
			Type:   sandbox.MountBind,
			IsFile: true,
		})
		env["BOID_BROKER_SOCKET"] = "/run/boid/broker.sock"
	}
	if spec.BrokerToken != "" {
		env["BOID_BROKER_TOKEN"] = spec.BrokerToken
	}

	// --- Files (context files) ---
	var files []sandbox.FileWrite
	contextDir := homeDir + "/.boid/context"
	if spec.TaskYAML != "" {
		files = append(files, sandbox.FileWrite{
			Path: contextDir + "/task.yaml", Content: spec.TaskYAML,
		})
	}
	if spec.EnvironmentYAML != "" {
		files = append(files, sandbox.FileWrite{
			Path: contextDir + "/environment.yaml", Content: spec.EnvironmentYAML,
		})
	}
	if spec.InstructionsJSON != "" {
		files = append(files, sandbox.FileWrite{
			Path: contextDir + "/instructions.yaml", Content: jsonToYAML(spec.InstructionsJSON),
		})
	}
	if spec.PayloadJSON != "" {
		files = append(files, sandbox.FileWrite{
			Path: contextDir + "/payload.yaml", Content: jsonToYAML(spec.PayloadJSON),
		})
		files = append(files, sandbox.FileWrite{
			Path: contextDir + "/payload.json", Content: spec.PayloadJSON,
		})
	}

	// --- Role-specific layout ---
	var argv []string
	var stdinBytes []byte
	var stdoutCapture string
	var exitScript string

	switch spec.Role {
	case "hook":
		mounts = append(mounts, projectMounts(spec.ProjectDir, workDir, homeDir, spec.WorktreeDir, spec.Readonly, spec.WorkspaceDirs)...)
		mounts = append(mounts, hookFileMounts(workDir, spec.ProjectDir, spec.HookFiles)...)
		mounts = append(mounts, additionalBindingMounts(spec.AdditionalBindings)...)
		argv = resolveHookArgv(spec, workDir)
		if !spec.Interactive {
			stdinBytes = []byte(spec.PayloadJSON)
		} else {
			env["BOID_INTERACTIVE"] = "1"
		}
		exitScript = buildExitScript(spec.JobID, "$HOME/.boid/output/payload_patch.yaml", "")
	case "gate":
		env["HOME"] = "/tmp"
		mounts = append(mounts, sandbox.Mount{Target: "/tmp", Type: sandbox.MountTmpfs})
		if workDir != "" {
			mounts = append(mounts, sandbox.Mount{Target: workDir, Type: sandbox.MountTmpfs})
		}
		// Stage gate script at /opt/boid/gates/<name>
		if spec.HookScript != "" {
			gatesDir := spec.GatesDir
			if gatesDir == "" {
				gatesDir = spec.ProjectDir + "/.boid/gates"
			}
			mounts = append(mounts, sandbox.Mount{
				Source: gatesDir + "/" + spec.HookScript,
				Target: "/opt/boid/gates/" + spec.HookScript,
				Type:   sandbox.MountBind,
				IsFile: true,
			})
		}
		mounts = append(mounts, additionalBindingMounts(spec.AdditionalBindings)...)
		argv = resolveGateArgv(spec)
		stdinBytes = []byte(spec.TaskJSON)
		stdoutCapture = "/tmp/boid-output"
		// Gate HOME is /tmp; ensure context/output dir ref resolves there.
		exitScript = buildExitScript(spec.JobID, "$HOME/.boid/output/payload_patch.yaml", "/tmp/boid-output")
	default:
		// Exec / command mode: full project access, server socket direct, no exit trap.
		mounts = append(mounts, projectMounts(spec.ProjectDir, workDir, homeDir, spec.WorktreeDir, false, spec.WorkspaceDirs)...)
		mounts = append(mounts, additionalBindingMounts(spec.AdditionalBindings)...)
		if spec.ServerSocket != "" {
			mounts = append(mounts, sandbox.Mount{
				Source: spec.ServerSocket,
				Target: "/run/boid/server.sock",
				Type:   sandbox.MountBind,
				IsFile: true,
			})
			env["BOID_JOB_ID"] = spec.JobID
			env["BOID_SOCKET"] = "/run/boid/server.sock"
		}
		argv = spec.Argv
	}

	// --- Boid binary + command shims ---
	mounts = append(mounts, sandbox.Mount{
		Source:   spec.BoidBinary,
		Target:   "/opt/boid/bin/boid",
		Type:     sandbox.MountBind,
		IsFile:   true,
		ReadOnly: true,
	})
	symlinks := shimSymlinks(sortedPolicyKeys(spec.BuiltinPolicies), spec.HostCommands)

	// --- Cleanup paths ---
	var cleanup []string
	if spec.StagingDir != "" {
		cleanup = append(cleanup, spec.StagingDir)
	}

	return sandbox.Spec{
		ID:                spec.JobID,
		Mounts:            mounts,
		Files:             files,
		Symlinks:          symlinks,
		ProxyPort:         spec.ProxyPort,
		Argv:              argv,
		WorkDir:           workDir,
		Env:               env,
		StdinBytes:        stdinBytes,
		StdoutCaptureFile: stdoutCapture,
		ExitScript:        exitScript,
		TTY:               spec.TTY,
		RootDir:           rootDir,
		CleanupPaths:      cleanup,
	}
}

// projectMounts returns the standard filesystem layout for a job that sees
// the project: project bind → HOME tmpfs → project re-mount → peers (ro) →
// .boid (ro) → (.git remount in worktree mode).
func projectMounts(projectDir, workDir, homeDir, worktreeDir string, readOnly bool, workspacePeers map[string]string) []sandbox.Mount {
	var out []sandbox.Mount

	// Project bind-mount before HOME tmpfs so the path exists inside the sandbox.
	out = append(out, sandbox.Mount{
		Source:   workDir,
		Target:   workDir,
		Type:     sandbox.MountBind,
		ReadOnly: readOnly,
	})

	// HOME tmpfs (caller decides homeDir).
	out = append(out, sandbox.Mount{
		Target: homeDir,
		Type:   sandbox.MountTmpfs,
	})

	// Re-mount project after HOME tmpfs so it stays visible when workDir is
	// a descendant of homeDir (which the HOME tmpfs would otherwise shadow).
	out = append(out, sandbox.Mount{
		Source:   workDir,
		Target:   workDir,
		Type:     sandbox.MountBind,
		ReadOnly: readOnly,
	})

	// Workspace peers (read-only mirrors of other projects).
	peerKeys := sortedKeys(workspacePeers)
	for _, k := range peerKeys {
		out = append(out, sandbox.Mount{
			Source:   workspacePeers[k],
			Target:   workspacePeers[k],
			Type:     sandbox.MountBind,
			ReadOnly: true,
		})
	}

	// .boid (ro). In worktree mode the source stays at the original project dir.
	boidSource := projectDir + "/.boid"
	out = append(out, sandbox.Mount{
		Source:   boidSource,
		Target:   workDir + "/.boid",
		Type:     sandbox.MountBind,
		ReadOnly: true,
		Guard:    dirGuardExpr(boidSource),
	})

	// Worktree mode: the sandbox also needs .git at the original path so the
	// worktree's gitlink resolves.
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

// hookFileMounts returns the tmpfs-over-.boid/hooks pattern: a tmpfs layer so
// individual hook files can be bind-mounted on top of the read-only .boid dir.
// Called only when HookFiles is non-empty.
func hookFileMounts(workDir, projectDir string, files []HookFile) []sandbox.Mount {
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

// additionalBindingMounts converts dispatcher AdditionalBindings into Mount
// entries. Supports Target override and IsFile flag.
func additionalBindingMounts(bindings []BindMount) []sandbox.Mount {
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

// shimSymlinks creates /opt/boid/bin/<cmd> → boid symlinks for both builtin
// and host command names. The boid binary itself is skipped since it is
// bind-mounted directly at /opt/boid/bin/boid.
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
func buildPATH(bindings []BindMount) string {
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

// buildExitScript renders a shell snippet that calls `boid job done <jobID>`
// with --exit-code and, if the payload file exists, --output-file pointing at
// it. Optional stdoutFallback is used when the payload is absent.
func buildExitScript(jobID, payloadFile, stdoutFallback string) string {
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

// quoteForTrap wraps a path in double quotes so shell expansions ($HOME etc.)
// fire at trap execution time. The enclosing trap string is single-quoted.
func quoteForTrap(s string) string {
	return "\"" + s + "\""
}

// dirGuardExpr builds `-d <quoted-path>` for Mount.Guard, suitable for
// `if [ <expr> ]; then` wrapping.
func dirGuardExpr(dir string) string {
	return "-d " + shellQuoteDir(dir)
}

// shellQuoteDir is a small subset of shell quoting suitable for paths; we
// defer to sandbox.shellQuote at render time, but we need the quoted form
// inline in Guard expressions. Since dispatcher cannot import unexported
// sandbox helpers, we re-implement the safe-chars logic here.
func shellQuoteDir(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-"
	for _, r := range s {
		if !strings.ContainsRune(safe, r) {
			return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
		}
	}
	return s
}

func effectiveWorkDir(spec SandboxSpec) string {
	if spec.WorktreeDir != "" {
		return spec.WorktreeDir
	}
	return spec.ProjectDir
}

func effectiveHomeDir(spec SandboxSpec) string {
	if spec.HomeDir != "" {
		return spec.HomeDir
	}
	return spec.ProjectDir
}

func resolveHookArgv(spec SandboxSpec, workDir string) []string {
	if len(spec.Argv) > 0 {
		return spec.Argv
	}
	if spec.HookScript == "" {
		return nil
	}
	return []string{workDir + "/.boid/hooks/" + spec.HookScript}
}

func resolveGateArgv(spec SandboxSpec) []string {
	if len(spec.Argv) > 0 {
		return spec.Argv
	}
	if spec.HookScript == "" {
		return nil
	}
	return []string{"/opt/boid/gates/" + spec.HookScript}
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

// sortedPolicyKeys extracts the builtin command names from a policy map and
// returns them as a sorted slice for deterministic shim creation.
func sortedPolicyKeys(policies map[string]sandbox.BuiltinPolicy) []string {
	return sortedKeys(policies)
}

// jsonToYAML converts a JSON string to YAML. Falls back to the original on
// parse error. Replaces the sandbox.jsonToYAML helper that used to live inside
// the sandbox layer.
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

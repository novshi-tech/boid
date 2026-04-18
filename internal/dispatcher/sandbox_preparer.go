package dispatcher

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
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

	wc := translateSpecToWrapperConfig(spec, rootDir)

	outerPath, err := sandbox.WriteSandboxScripts(wc)
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

// translateSpecToWrapperConfig maps the dispatcher-layer spec (which still
// carries Role / Broker / ServerSocket concepts) down to primitive-only
// WrapperConfig. This Role → primitive translation is the role-aware seam
// that M5 will relocate into orchestrator.
func translateSpecToWrapperConfig(spec SandboxSpec, rootDir string) sandbox.WrapperConfig {
	wc := sandbox.WrapperConfig{
		JobID:              spec.JobID,
		TaskID:             spec.TaskID,
		ProjectID:          spec.ProjectID,
		ProjectDir:         spec.ProjectDir,
		HomeDir:            spec.HomeDir,
		HookFiles:          toSandboxHookFiles(spec.HookFiles),
		Argv:               spec.Argv,
		BoidBinary:         spec.BoidBinary,
		Env:                cloneEnv(spec.Env),
		BuiltinCommands:    sortedPolicyKeys(spec.BuiltinPolicies),
		HostCommands:       spec.HostCommands,
		AdditionalBindings: toSandboxBindMounts(spec.AdditionalBindings),
		WorkspaceDirs:      spec.WorkspaceDirs,
		ProxyPort:          spec.ProxyPort,
		StagingDir:         spec.StagingDir,
		RootDir:            rootDir,
		TTY:                spec.TTY,
		WorktreeDir:        spec.WorktreeDir,
		InstructionsJSON:   spec.InstructionsJSON,
		TaskYAML:           spec.TaskYAML,
		EnvironmentYAML:    spec.EnvironmentYAML,
		Model:              spec.Model,
		InvokedRole:        spec.InvokedRole,
		InvokedName:        spec.InvokedName,
		InvokedType:        spec.InvokedType,
	}

	// Broker socket + token are exposed as a mount + env entry; sandbox
	// layer has no dedicated Broker concept.
	if spec.BrokerSocket != "" {
		wc.AdditionalBindings = append(wc.AdditionalBindings, sandbox.BindMount{
			Source: spec.BrokerSocket,
			Target: "/run/boid/broker.sock",
			IsFile: true,
		})
		if wc.Env == nil {
			wc.Env = map[string]string{}
		}
		wc.Env["BOID_BROKER_SOCKET"] = "/run/boid/broker.sock"
	}
	if spec.BrokerToken != "" {
		if wc.Env == nil {
			wc.Env = map[string]string{}
		}
		wc.Env["BOID_BROKER_TOKEN"] = spec.BrokerToken
	}

	switch spec.Role {
	case "hook":
		wc.MountProjectDir = true
		wc.ProjectReadOnly = spec.Readonly
		wc.PayloadJSON = spec.PayloadJSON
		if !spec.Interactive {
			wc.StdinBytes = []byte(spec.PayloadJSON)
		} else if wc.Env == nil {
			wc.Env = map[string]string{"BOID_INTERACTIVE": "1"}
		} else {
			wc.Env["BOID_INTERACTIVE"] = "1"
		}
		wc.Argv = resolveHookArgv(spec)
		wc.ExitScript = buildPayloadExitScript(spec.JobID, "$HOME/.boid/output/payload_patch.yaml", "")
	case "gate":
		wc.MountProjectDir = false
		wc.HomeDir = "/tmp"
		wc.StdinBytes = []byte(spec.TaskJSON)
		wc.StdoutCaptureFile = "/tmp/boid-output"
		wc.Argv = resolveGateArgv(spec)
		// Stage the gate script into sandbox via additional binding.
		if spec.HookScript != "" {
			gatesDir := spec.GatesDir
			if gatesDir == "" {
				gatesDir = spec.ProjectDir + "/.boid/gates"
			}
			wc.AdditionalBindings = append(wc.AdditionalBindings, sandbox.BindMount{
				Source: gatesDir + "/" + spec.HookScript,
				Target: "/opt/boid/gates/" + spec.HookScript,
				IsFile: true,
			})
		}
		wc.ExitScript = buildPayloadExitScript(spec.JobID, "$HOME/.boid/output/payload_patch.yaml", "/tmp/boid-output")
	default:
		// Exec / command mode: server socket bind + env, no trap, shell replaced via `exec`.
		wc.MountProjectDir = true
		wc.ProjectReadOnly = false
		if spec.ServerSocket != "" {
			wc.AdditionalBindings = append(wc.AdditionalBindings, sandbox.BindMount{
				Source: spec.ServerSocket,
				Target: "/run/boid/server.sock",
				IsFile: true,
			})
			if wc.Env == nil {
				wc.Env = map[string]string{}
			}
			wc.Env["BOID_JOB_ID"] = spec.JobID
			wc.Env["BOID_SOCKET"] = "/run/boid/server.sock"
		}
		// ExitScript stays empty so the entry is rendered with `exec`.
	}
	return wc
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func resolveHookArgv(spec SandboxSpec) []string {
	if len(spec.Argv) > 0 {
		return spec.Argv
	}
	if spec.HookScript == "" {
		return nil
	}
	wd := spec.WorktreeDir
	if wd == "" {
		wd = spec.ProjectDir
	}
	return []string{wd + "/.boid/hooks/" + spec.HookScript}
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

// buildPayloadExitScript renders a shell snippet that calls
// `boid job done <jobID>` with `--exit-code` and, when the expected payload
// file exists, `--output-file <payload>`. When payloadFile is absent but
// stdoutFallback is set, it is used as the output file. Otherwise only the
// exit code is passed.
func buildPayloadExitScript(jobID, payloadFile, stdoutFallback string) string {
	var b strings.Builder
	b.WriteString("_exit=$?\n")
	fmt.Fprintf(&b, "mkdir -p \"$(dirname %s)\"\n", shellQuoteForTrap(payloadFile))
	fmt.Fprintf(&b, "if [ -f %s ]; then\n", shellQuoteForTrap(payloadFile))
	fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit --output-file %s\n", jobID, shellQuoteForTrap(payloadFile))
	if stdoutFallback != "" {
		b.WriteString("else\n")
		fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit --output-file %s\n", jobID, shellQuoteForTrap(stdoutFallback))
	} else {
		b.WriteString("else\n")
		fmt.Fprintf(&b, "  boid job done %s --exit-code $_exit\n", jobID)
	}
	b.WriteString("fi")
	return b.String()
}

// shellQuoteForTrap renders a path safely for inclusion inside a string that
// will itself be passed through sandbox.shellQuote (single-quoted). We use
// double quotes with escaped expansions to keep $HOME live at trap time.
func shellQuoteForTrap(s string) string {
	return "\"" + s + "\""
}

// sortedPolicyKeys extracts the builtin command names from a policy map and
// returns them as a sorted slice for deterministic shim creation.
func sortedPolicyKeys(policies map[string]sandbox.BuiltinPolicy) []string {
	if len(policies) == 0 {
		return nil
	}
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func toSandboxHookFiles(files []HookFile) []sandbox.HookFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]sandbox.HookFile, len(files))
	for i, f := range files {
		out[i] = sandbox.HookFile{Source: f.Source, TargetName: f.TargetName}
	}
	return out
}

func toSandboxBindMounts(bindings []BindMount) []sandbox.BindMount {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]sandbox.BindMount, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, sandbox.BindMount{
			Source: binding.Source,
			Target: binding.Target,
			Mode:   binding.Mode,
			IsFile: binding.IsFile,
		})
	}
	return out
}

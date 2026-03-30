package job

import (
	"fmt"
	"os"
	"strings"

	"github.com/novshi-tech/boid/internal/model"
)

// WrapperConfig holds the parameters for sandbox script generation.
type WrapperConfig struct {
	JobID              string
	TaskID             string
	ProjectID          string
	ProjectDir         string            // host-side project directory
	HomeDir            string            // host-side user home directory (fallback to ProjectDir)
	HooksDir           string            // host-side hooks directory
	HookScript         string            // script filename, e.g. "run-build.sh"
	Command            string            // command to execute (non-interactive, non-hook mode)
	BoidBinary         string            // host-side path to boid binary
	ServerSocket       string            // host-side server socket path
	BrokerSocket       string            // host-side broker socket path
	BrokerToken        string            // broker authentication token
	Env                map[string]string // project environment variables
	HostCommands       []string          // command names to shim via symlinks
	AdditionalBindings []model.BindMount // extra host paths to bind-mount
	WorkspaceDirs      map[string]string // project-id -> host-dir (read-only mounts)
	ProxyPort          int               // host-side proxy port (0 = no proxy)
	StagingDir         string            // if set, staging dir to clean up after job
	TTY                bool              // if true, preserve TTY through pasta (for interactive commands)
	WorktreeDir        string            // if set, worktree mode: sandbox works here; .git/.boid come from ProjectDir
	Role               string            // "hook", "gate", or "" (legacy/command mode)
	PayloadJSON        string            // task payload JSON for hook stdin
	TaskJSON           string            // full task data JSON for gate stdin
	Readonly           bool              // if true, mount working dir as read-only
}

// workDir returns the effective working directory inside the sandbox.
// In worktree mode this is WorktreeDir; otherwise ProjectDir.
func (cfg WrapperConfig) workDir() string {
	if cfg.WorktreeDir != "" {
		return cfg.WorktreeDir
	}
	return cfg.ProjectDir
}

// homeDir returns the effective home directory.
func (cfg WrapperConfig) homeDir() string {
	if cfg.HomeDir != "" {
		return cfg.HomeDir
	}
	return cfg.ProjectDir
}

// WriteSandboxScripts generates 3 sandbox scripts and writes them to /tmp.
// Returns the path to the outer script that should be executed in tmux.
func WriteSandboxScripts(cfg WrapperConfig) (string, error) {
	prefix := fmt.Sprintf("/tmp/boid-%s", cfg.JobID)

	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	outerPath := prefix + "-outer.sh"

	inner := generateInnerScript(cfg)
	plan := BuildSandboxPlan(cfg)
	setup := RenderSetupScript(plan, innerPath, setupPath, outerPath)
	outer := generateOuterScript(cfg, setupPath)

	for _, f := range []struct{ path, content string }{
		{innerPath, inner},
		{setupPath, setup},
		{outerPath, outer},
	} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o755); err != nil {
			return "", fmt.Errorf("write %s: %w", f.path, err)
		}
	}

	return outerPath, nil
}

func generateOuterScript(cfg WrapperConfig, setupPath string) string {
	if cfg.TTY {
		// Save original stderr to fd 3, suppress pasta's warnings,
		// then restore stderr in the child so the TTY is preserved.
		return fmt.Sprintf(`#!/bin/bash
set -e
exec 3>&2
exec pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    2>/dev/null \
    -- bash -c 'exec 2>&3 3>&-; exec unshare --mount -- bash %s'
`, setupPath)
	}
	return fmt.Sprintf(`#!/bin/bash
set -e
exec pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    2>/dev/null \
    -- unshare --mount -- bash %s
`, setupPath)
}


// additionalPATH builds PATH entries from additional bindings.
// Paths ending in /bin are added directly; others get /bin appended.
func additionalPATH(bindings []model.BindMount) string {
	var parts []string
	for _, bm := range bindings {
		if strings.HasSuffix(bm.Source, "/bin") {
			parts = append(parts, bm.Source)
		} else {
			parts = append(parts, bm.Source+"/bin")
		}
	}
	return strings.Join(parts, ":")
}

func generateInnerScript(cfg WrapperConfig) string {
	switch cfg.Role {
	case "hook":
		return generateHookInnerScript(cfg)
	case "gate":
		return generateGateInnerScript(cfg)
	default:
		return generateLegacyInnerScript(cfg)
	}
}

// generateHookInnerScript creates the inner script for hook execution.
// Only BOID_BROKER_TOKEN is exported. Payload is piped via stdin.
// Stdout is captured to /tmp/boid-output for payload_patch.
func generateHookInnerScript(cfg WrapperConfig) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")

	fmt.Fprintf(&b, "export HOME=%s\n", cfg.homeDir())

	if cfg.BrokerToken != "" {
		fmt.Fprintf(&b, "export BOID_BROKER_TOKEN=%s\n", cfg.BrokerToken)
	}
	if cfg.BrokerSocket != "" {
		b.WriteString("export BOID_BROKER_SOCKET=/run/boid/broker.sock\n")
	}

	writePathAndProxy(&b, cfg)

	wd := cfg.workDir()
	fmt.Fprintf(&b, "\ncd %s\n\n", wd)

	b.WriteString("trap 'boid job done --exit-code $? --output-file /tmp/boid-output' EXIT\n")
	fmt.Fprintf(&b, "echo '%s' | %s/.boid/hooks/%s > /tmp/boid-output\n", cfg.PayloadJSON, wd, cfg.HookScript)

	return b.String()
}

// generateGateInnerScript creates the inner script for gate execution.
// No filesystem access. Task data is piped via stdin.
func generateGateInnerScript(cfg WrapperConfig) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")

	fmt.Fprintf(&b, "export HOME=/tmp\n")

	if cfg.BrokerToken != "" {
		fmt.Fprintf(&b, "export BOID_BROKER_TOKEN=%s\n", cfg.BrokerToken)
	}
	if cfg.BrokerSocket != "" {
		b.WriteString("export BOID_BROKER_SOCKET=/run/boid/broker.sock\n")
	}

	writePathAndProxy(&b, cfg)

	b.WriteString("\ncd /tmp\n\n")

	b.WriteString("trap 'boid job done --exit-code $? --output-file /tmp/boid-output' EXIT\n")
	fmt.Fprintf(&b, "echo '%s' | %s > /tmp/boid-output\n", cfg.TaskJSON, cfg.workDir()+"/.boid/gates/"+cfg.HookScript)

	return b.String()
}

// generateLegacyInnerScript creates the inner script for legacy/command mode.
// This preserves backward compatibility with existing behavior.
func generateLegacyInnerScript(cfg WrapperConfig) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")

	homeDir := cfg.homeDir()
	fmt.Fprintf(&b, "export HOME=%s\n", homeDir)

	if cfg.TaskID != "" {
		fmt.Fprintf(&b, "export BOID_TASK_ID=%s\n", cfg.TaskID)
	}
	fmt.Fprintf(&b, "export BOID_JOB_ID=%s\n", cfg.JobID)

	b.WriteString("export BOID_SOCKET=/run/boid/server.sock\n")
	if cfg.BrokerSocket != "" {
		b.WriteString("export BOID_BROKER_SOCKET=/run/boid/broker.sock\n")
	}
	if cfg.BrokerToken != "" {
		fmt.Fprintf(&b, "export BOID_BROKER_TOKEN=%s\n", cfg.BrokerToken)
	}

	writePathAndProxy(&b, cfg)

	wd := cfg.workDir()
	fmt.Fprintf(&b, "\ncd %s\n\n", wd)

	if cfg.Command != "" {
		fmt.Fprintf(&b, "exec %s\n", cfg.Command)
	} else {
		fmt.Fprintf(&b, "trap 'boid job done %s --exit-code $?' EXIT\n", cfg.JobID)
		fmt.Fprintf(&b, "%s/.boid/hooks/%s\n", wd, cfg.HookScript)
	}

	return b.String()
}

// writePathAndProxy writes PATH and proxy environment variables.
func writePathAndProxy(b *strings.Builder, cfg WrapperConfig) {
	pathPrefix := additionalPATH(cfg.AdditionalBindings)
	basePath := "/opt/boid/bin:/usr/local/bin:/usr/bin:/bin"
	if pathPrefix != "" {
		fmt.Fprintf(b, "export PATH=%s:%s\n", pathPrefix, basePath)
	} else {
		fmt.Fprintf(b, "export PATH=%s\n", basePath)
	}

	if cfg.ProxyPort > 0 {
		proxyURL := fmt.Sprintf("http://10.0.2.2:%d", cfg.ProxyPort)
		fmt.Fprintf(b, "export http_proxy=%s\n", proxyURL)
		fmt.Fprintf(b, "export https_proxy=%s\n", proxyURL)
		fmt.Fprintf(b, "export HTTP_PROXY=%s\n", proxyURL)
		fmt.Fprintf(b, "export HTTPS_PROXY=%s\n", proxyURL)
		b.WriteString("export no_proxy=10.0.2.2,10.0.2.3,localhost,127.0.0.1\n")
		b.WriteString("export NO_PROXY=10.0.2.2,10.0.2.3,localhost,127.0.0.1\n")
	}

	for k, v := range cfg.Env {
		fmt.Fprintf(b, "export %s=%q\n", k, v)
	}
}

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// WrapperConfig holds the parameters for sandbox script generation.
// This layer knows only primitives: no Role, no Job, no Broker, no Gate.
type WrapperConfig struct {
	JobID      string
	TaskID     string
	ProjectID  string
	ProjectDir string     // host-side project directory
	HomeDir    string     // where HOME tmpfs is mounted (and exported as HOME); "" falls back to ProjectDir
	HookFiles  []HookFile // individual hook files to bind-mount under WorkDir/.boid/hooks/
	Argv       []string   // program + args to execute inside the sandbox
	BoidBinary string     // host-side path to boid binary (bind-mounted at /opt/boid/bin/boid)
	Env        map[string]string
	BuiltinCommands    []string
	HostCommands       []string
	AdditionalBindings []BindMount       // extra host paths to bind-mount (supports Target/IsFile/Mode)
	WorkspaceDirs      map[string]string // project-id -> host-dir (read-only mounts, only when MountProjectDir=true)
	ProxyPort          int
	StagingDir         string
	RootDir            string
	TTY                bool
	WorktreeDir        string

	// MountProjectDir: bind-mount ProjectDir (or WorktreeDir) at WorkDir.
	// When false, an empty tmpfs is mounted at WorkDir so `cd` works.
	MountProjectDir bool
	// ProjectReadOnly: when MountProjectDir is true, mount read-only.
	ProjectReadOnly bool

	// StdinBytes: content piped into the entry process's stdin. nil/empty = inherit.
	StdinBytes []byte
	// StdoutCaptureFile: sandbox-internal path to redirect stdout to. "" = inherit.
	StdoutCaptureFile string
	// ExitScript: shell snippet wrapped in `trap '<script>' EXIT`. "" = no trap
	// (for `exec`-style entry that replaces the shell).
	ExitScript string

	// Context files written to $HOME/.boid/context/ before entry runs. M4 collapses these into generic files.
	InstructionsJSON string // BOID_INSTRUCTIONS env
	TaskYAML         string // context/task.yaml
	EnvironmentYAML  string // context/environment.yaml
	PayloadJSON      string // context/payload.yaml (and context/payload.json for convenience)
	Model            string // BOID_MODEL env
	InvokedRole      string // BOID_INVOKED_ROLE env
	InvokedName      string // BOID_INVOKED_NAME env
	InvokedType      string // BOID_INVOKED_TYPE env
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
// Returns the path to the outer script that should be executed by the job runtime.
func WriteSandboxScripts(cfg WrapperConfig) (string, error) {
	prefix := fmt.Sprintf("/tmp/boid-%s", cfg.JobID)

	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	outerPath := prefix + "-outer.sh"

	inner := generateInnerScript(cfg)
	plan := BuildSandboxPlan(cfg)
	setup := RenderSetupScript(plan, cfg.RootDir, innerPath, setupPath, outerPath)
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
func additionalPATH(bindings []BindMount) string {
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

// generateInnerScript builds the script that runs inside the sandbox.
// Behavior is driven entirely by WrapperConfig primitives — no Role,
// no Job, no Broker concept appears here. Role-specific patterns (hook,
// gate, exec) are expressed by the caller as primitive combinations.
func generateInnerScript(cfg WrapperConfig) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n\n")

	// HOME is always a tmpfs at cfg.HomeDir; export it so `cd $HOME` and
	// similar work. Caller chooses the HomeDir (e.g. /tmp for gate-like jobs).
	fmt.Fprintf(&b, "export HOME=%s\n", shellQuote(cfg.homeDir()))

	if cfg.TaskID != "" {
		fmt.Fprintf(&b, "export BOID_TASK_ID=%s\n", shellQuote(cfg.TaskID))
	}
	// M4 cleanup target: move the following to cfg.Env entries and remove
	// these dedicated fields from WrapperConfig.
	if cfg.InstructionsJSON != "" {
		fmt.Fprintf(&b, "export BOID_INSTRUCTIONS=%s\n", shellQuote(cfg.InstructionsJSON))
	}
	if cfg.Model != "" {
		fmt.Fprintf(&b, "export BOID_MODEL=%s\n", shellQuote(cfg.Model))
	}
	if cfg.InvokedRole != "" || cfg.InvokedName != "" || cfg.InvokedType != "" {
		fmt.Fprintf(&b, "export BOID_INVOKED_ROLE=%s\n", shellQuote(cfg.InvokedRole))
		fmt.Fprintf(&b, "export BOID_INVOKED_NAME=%s\n", shellQuote(cfg.InvokedName))
		fmt.Fprintf(&b, "export BOID_INVOKED_TYPE=%s\n", shellQuote(cfg.InvokedType))
	}
	writeBuiltinShimEnv(&b, cfg)

	writePathAndProxy(&b, cfg)

	// Context files written at $HOME/.boid/context/. No-op if no content set.
	writeContextFiles(&b, cfg)

	wd := cfg.workDir()
	fmt.Fprintf(&b, "\ncd %s\n\n", shellQuote(wd))

	// EXIT trap: opaque shell snippet provided by caller. Empty means no trap
	// (appropriate for `exec`-style entries that replace the shell).
	if cfg.ExitScript != "" {
		fmt.Fprintf(&b, "trap %s EXIT\n", shellQuote(cfg.ExitScript))
	}

	// Entry composition:
	//   StdinBytes  StdoutCaptureFile  ExitScript  →  rendered form
	//   yes         yes                 -          →  printf '%s' <bytes> | <argv> > <file>
	//   yes         no                  -          →  printf '%s' <bytes> | <argv>
	//   no          -                   ""         →  exec <argv>            (replace shell)
	//   no          -                   non-""     →  <argv>                 (trap fires on exit)
	quoted := shellQuoteArgv(cfg.Argv)
	switch {
	case len(cfg.StdinBytes) > 0 && cfg.StdoutCaptureFile != "":
		fmt.Fprintf(&b, "printf '%%s' %s | %s > %s\n",
			shellQuote(string(cfg.StdinBytes)), quoted, shellQuote(cfg.StdoutCaptureFile))
	case len(cfg.StdinBytes) > 0:
		fmt.Fprintf(&b, "printf '%%s' %s | %s\n",
			shellQuote(string(cfg.StdinBytes)), quoted)
	case cfg.ExitScript == "":
		fmt.Fprintf(&b, "exec %s\n", quoted)
	default:
		fmt.Fprintf(&b, "%s\n", quoted)
	}

	return b.String()
}

// shellQuoteArgv renders []string as a space-separated sequence of
// individually shell-quoted tokens. This is the single render point
// where argv converts to shell syntax.
func shellQuoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// writePathAndProxy writes PATH and proxy environment variables.
func writePathAndProxy(b *strings.Builder, cfg WrapperConfig) {
	pathPrefix := additionalPATH(cfg.AdditionalBindings)
	basePath := "/opt/boid/bin:/usr/local/bin:/usr/bin:/bin"
	if pathPrefix != "" {
		fmt.Fprintf(b, "export PATH=%s\n", shellQuote(pathPrefix+":"+basePath))
	} else {
		fmt.Fprintf(b, "export PATH=%s\n", shellQuote(basePath))
	}

	b.WriteString("export TERM=xterm-256color\n")

	if cfg.ProxyPort > 0 {
		proxyURL := fmt.Sprintf("http://10.0.2.2:%d", cfg.ProxyPort)
		fmt.Fprintf(b, "export http_proxy=%s\n", shellQuote(proxyURL))
		fmt.Fprintf(b, "export https_proxy=%s\n", shellQuote(proxyURL))
		fmt.Fprintf(b, "export HTTP_PROXY=%s\n", shellQuote(proxyURL))
		fmt.Fprintf(b, "export HTTPS_PROXY=%s\n", shellQuote(proxyURL))
		b.WriteString("export no_proxy=10.0.2.2,10.0.2.3,localhost,127.0.0.1\n")
		b.WriteString("export NO_PROXY=10.0.2.2,10.0.2.3,localhost,127.0.0.1\n")
	}

	for k, v := range cfg.Env {
		fmt.Fprintf(b, "export %s=%q\n", k, v)
	}
}

// writeContextFiles generates ~/.boid/context/ files inside the sandbox.
// No-op when none of the context fields are populated.
// M4 target: replace these dedicated fields with generic Files[] primitives.
func writeContextFiles(b *strings.Builder, cfg WrapperConfig) {
	if cfg.TaskYAML == "" && cfg.InstructionsJSON == "" && cfg.PayloadJSON == "" && cfg.EnvironmentYAML == "" {
		return
	}
	contextDir := cfg.homeDir() + "/.boid/context"
	fmt.Fprintf(b, "\nmkdir -p %s\n", shellQuote(contextDir))

	if cfg.TaskYAML != "" {
		fmt.Fprintf(b, "printf '%%s' %s > %s/task.yaml\n", shellQuote(cfg.TaskYAML), shellQuote(contextDir))
	}
	if cfg.InstructionsJSON != "" {
		fmt.Fprintf(b, "printf '%%s' %s > %s/instructions.yaml\n", shellQuote(jsonToYAML(cfg.InstructionsJSON)), shellQuote(contextDir))
	}
	if cfg.PayloadJSON != "" {
		fmt.Fprintf(b, "printf '%%s' %s > %s/payload.yaml\n", shellQuote(jsonToYAML(cfg.PayloadJSON)), shellQuote(contextDir))
		fmt.Fprintf(b, "printf '%%s' %s > %s/payload.json\n", shellQuote(cfg.PayloadJSON), shellQuote(contextDir))
	}
	if cfg.EnvironmentYAML != "" {
		fmt.Fprintf(b, "printf '%%s' %s > %s/environment.yaml\n", shellQuote(cfg.EnvironmentYAML), shellQuote(contextDir))
	}
}

// jsonToYAML converts a JSON string to YAML. Falls back to the original string on error.
func jsonToYAML(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
}

func writeBuiltinShimEnv(b *strings.Builder, cfg WrapperConfig) {
	for _, name := range cfg.BuiltinCommands {
		if name == "boid" {
			b.WriteString("export BOID_BUILTIN_SHIM=1\n")
			return
		}
	}
}

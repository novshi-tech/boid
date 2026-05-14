package sandbox

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Spec describes a sandbox invocation in primitives only. The sandbox
// layer knows nothing about Role / Task / Job / Broker / Gate / Hook —
// all of those are the caller's concern. Everything needed to build and
// run the sandbox must already be present as mounts, files, env, argv, etc.
type Spec struct {
	// ID is used to namespace the generated /tmp/boid-<ID>-{outer,setup,inner}.sh files.
	ID string

	// --- Filesystem primitives ---
	// Mounts are applied in order to compose the sandbox root filesystem.
	Mounts []Mount
	// Files are materialized inside the sandbox before the entry command runs.
	Files []FileWrite
	// Symlinks are created inside the sandbox (e.g. /opt/boid/bin/<cmd> → boid).
	Symlinks []Symlink

	// --- Network ---
	// ProxyPort, when > 0, engages the nft drop policy + HTTP proxy env vars.
	ProxyPort int

	// --- Process ---
	// Argv is the program and arguments to invoke (POSIX argv).
	Argv []string
	// WorkDir is the cwd for the entry process.
	WorkDir string
	// Env is exported before the entry command runs.
	Env map[string]string
	// StdinBytes, when non-empty, is piped into the entry's stdin.
	StdinBytes []byte
	// StdoutCaptureFile, when non-empty, redirects stdout to that sandbox-internal path.
	StdoutCaptureFile string
	// ExitScript is wrapped in `trap '<ExitScript>' EXIT`. Empty = no trap (suits exec-replaced shells).
	ExitScript string
	// TTY, when true, preserves the caller's TTY through pasta so Argv can run as a full terminal app.
	TTY bool

	// --- Bookkeeping ---
	// RootDir, if non-empty, is used as the sandbox ROOT directory so Go-side
	// cleanup can remove it after exit. If empty, setup script creates one with mktemp.
	RootDir string
	// CleanupPaths are removed by the setup script's EXIT trap (used for staging dirs).
	CleanupPaths []string
}

// Prepare writes the 3 scripts (outer / setup / inner) to /tmp and returns
// the path to the outer script. The caller is responsible for actually running
// it (via exec.Cmd, syscall.Exec, or a runtime abstraction).
func Prepare(spec Spec) (string, error) {
	prefix := fmt.Sprintf("/tmp/boid-%s", spec.ID)

	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	outerPath := prefix + "-outer.sh"

	inner := generateInnerScript(spec)
	plan := buildPlan(spec)
	setup := renderSetupScript(plan, spec.RootDir, innerPath, setupPath, outerPath)
	outer := generateOuterScript(spec, outerPath, setupPath, innerPath)

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

func generateOuterScript(spec Spec, outerPath, setupPath, innerPath string) string {
	rootDir := shellQuote(spec.RootDir)
	qOuter := shellQuote(outerPath)
	qSetup := shellQuote(setupPath)
	qInner := shellQuote(innerPath)
	if spec.TTY {
		return fmt.Sprintf(`#!/bin/bash
# Ignore SIGUSR1 — this is the daemon's "agent-stop" signal, meant for
# run-agent.py only. SIG_IGN propagates across execve(2) so pasta / unshare /
# inner bash all inherit this disposition and survive a process-group
# SIGUSR1 without dying. run-agent.py overrides via signal.signal() to act
# on it.
trap '' USR1
root_dir=%s
exec 3>&2
pasta_stderr=$(mktemp -t boid-pasta-stderr-XXXXXX.log)
pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    2>"$pasta_stderr" \
    -- bash -c 'exec 2>&3 3>&-; exec unshare --mount -- bash %s'
exit_code=$?
if [ "$exit_code" -ne 0 ] && [ -s "$pasta_stderr" ]; then
    echo "[boid] pasta stderr (exit_code=$exit_code):" >&2
    cat "$pasta_stderr" >&2
fi
rm -f "$pasta_stderr" 2>/dev/null || true
if [ "$exit_code" -eq 0 ]; then
    rm -rf "$root_dir" 2>/dev/null || true
    rm -f %s %s %s 2>/dev/null || true
fi
exit $exit_code
`, rootDir, setupPath, qOuter, qSetup, qInner)
	}
	return fmt.Sprintf(`#!/bin/bash
trap '' USR1
root_dir=%s
pasta_stderr=$(mktemp -t boid-pasta-stderr-XXXXXX.log)
pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    2>"$pasta_stderr" \
    -- unshare --mount -- bash %s
exit_code=$?
if [ "$exit_code" -ne 0 ] && [ -s "$pasta_stderr" ]; then
    echo "[boid] pasta stderr (exit_code=$exit_code):" >&2
    cat "$pasta_stderr" >&2
fi
rm -f "$pasta_stderr" 2>/dev/null || true
if [ "$exit_code" -eq 0 ]; then
    rm -rf "$root_dir" 2>/dev/null || true
    rm -f %s %s %s 2>/dev/null || true
fi
exit $exit_code
`, rootDir, setupPath, qOuter, qSetup, qInner)
}

// generateInnerScript builds the script that runs inside the sandbox.
// Behavior is driven entirely by Spec primitives — no Role, Job, Broker.
func generateInnerScript(spec Spec) string {
	var b strings.Builder

	b.WriteString("#!/bin/bash\nset -e\n")
	// Defense-in-depth: SIG_IGN should already be inherited from the outer
	// script across all the execve hops, but re-applying it here documents
	// the contract that only run-agent.py acts on SIGUSR1.
	b.WriteString("trap '' USR1\n\n")

	// Stable env ordering for deterministic script output.
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(spec.Env[k]))
	}

	// Files are written before cd, so relative targets (if any) resolve
	// against the sandbox root, not WorkDir.
	for _, f := range spec.Files {
		dir := filepathDir(f.Path)
		fmt.Fprintf(&b, "mkdir -p %s\n", shellQuote(dir))
		fmt.Fprintf(&b, "printf '%%s' %s > %s\n", shellQuote(f.Content), shellQuote(f.Path))
	}

	if spec.WorkDir != "" {
		fmt.Fprintf(&b, "\ncd %s\n\n", shellQuote(spec.WorkDir))
	}

	if spec.ExitScript != "" {
		fmt.Fprintf(&b, "trap %s EXIT\n", shellQuote(spec.ExitScript))
	}

	quoted := shellQuoteArgv(spec.Argv)
	switch {
	case len(spec.StdinBytes) > 0 && spec.StdoutCaptureFile != "":
		fmt.Fprintf(&b, "printf '%%s' %s | %s > %s\n",
			shellQuote(string(spec.StdinBytes)), quoted, shellQuote(spec.StdoutCaptureFile))
	case len(spec.StdinBytes) > 0:
		fmt.Fprintf(&b, "printf '%%s' %s | %s\n",
			shellQuote(string(spec.StdinBytes)), quoted)
	case spec.ExitScript == "":
		fmt.Fprintf(&b, "exec %s\n", quoted)
	default:
		fmt.Fprintf(&b, "%s\n", quoted)
	}

	return b.String()
}

// shellQuoteArgv renders []string as a space-separated sequence of
// individually shell-quoted tokens.
func shellQuoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// filepathDir returns the directory portion of a path. We avoid pulling in
// path/filepath for a single use and keep the logic explicit.
func filepathDir(p string) string {
	if idx := strings.LastIndex(p, "/"); idx > 0 {
		return p[:idx]
	}
	if strings.HasPrefix(p, "/") {
		return "/"
	}
	return "."
}

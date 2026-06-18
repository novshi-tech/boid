// Package runner is the go-native sandbox runner. It replaces the former bash
// trio (outer.sh / setup.sh / inner.sh): runner-outer launches pasta, which
// runs runner-inner (in pasta's user+net namespace), which clones
// runner-inner-child (CLONE_NEWUSER|CLONE_NEWNS) to lay out the mount namespace,
// pivot_root, and exec the agent.
//
// The syscall-heavy work lives in runner_linux.go; this file holds the portable
// helpers (spec decoding, pasta argv, signal mapping, guard evaluation) so they
// can be unit-tested off the syscall path.
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// readSpec loads a sandbox.Spec from the JSON file written by the dispatcher.
func readSpec(path string) (sandbox.Spec, error) {
	var spec sandbox.Spec
	data, err := os.ReadFile(path)
	if err != nil {
		return spec, fmt.Errorf("read runner spec %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("decode runner spec %q: %w", path, err)
	}
	return spec, nil
}

// pastaArgs returns the full pasta argv (excluding the binary name) that
// launches runner-inner inside a fresh user+net namespace. It mirrors the
// arguments the former outer.sh passed to pasta 1:1.
func pastaArgs(self, specPath, statePath string) []string {
	return []string{
		"--config-net",
		"-a", "10.0.2.0", "-n", "24", "-g", "10.0.2.2",
		"--dns-forward", "10.0.2.3",
		"-t", "none", "-u", "none",
		"--",
		self, "runner-inner", "--spec", specPath, "--state", statePath,
	}
}

// stopSignal maps the harness stop-signal name (Spec.StopSignalName, default
// "USR1") to the OS signal the runner sets to SIG_IGN. The former bash scripts
// did `trap ” <name>`; the go runner uses signal.Ignore so the disposition is
// inherited (SIG_IGN survives execve) by pasta and the child runners while the
// harness runner (run-agent.py) re-installs its own handler.
func stopSignal(spec sandbox.Spec) syscall.Signal {
	switch stopSignalName(spec) {
	case "USR1":
		return syscall.SIGUSR1
	case "USR2":
		return syscall.SIGUSR2
	case "TERM":
		return syscall.SIGTERM
	case "INT":
		return syscall.SIGINT
	case "HUP":
		return syscall.SIGHUP
	default:
		return syscall.SIGUSR1
	}
}

// ignoreStopSignal sets the harness stop signal to SIG_IGN for the current
// process. SIG_IGN is preserved across execve, so children inherit it.
func ignoreStopSignal(spec sandbox.Spec) {
	signal.Ignore(stopSignal(spec))
}

// envSlice converts the spec env map into the KEY=VALUE slice exec.Cmd wants,
// in sorted order for determinism.
func envSlice(env map[string]string) []string {
	keys := sortedEnvKeys(env)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// evalGuard evaluates a bash-style test expression of the form "-d PATH",
// "-e PATH" or "-f PATH" (PATH possibly single-quoted) against the host
// filesystem. An empty guard always passes. Unrecognised forms pass (fail-open)
// so a guard we don't understand never silently drops a mount.
//
// The operators follow symlinks (bash test semantics): /lib → /usr/lib on
// usrmerge hosts must satisfy "-d /lib".
func evalGuard(guard string) bool {
	if guard == "" {
		return true
	}
	op, rest, found := strings.Cut(guard, " ")
	if !found {
		return true
	}
	path := shellUnquote(strings.TrimSpace(rest))
	info, err := os.Stat(path)
	switch op {
	case "-d":
		return err == nil && info.IsDir()
	case "-f":
		return err == nil && info.Mode().IsRegular()
	case "-e":
		return err == nil
	default:
		return true
	}
}

// shellUnquote reverses the single-quote quoting produced by sandbox.shellQuote
// / shellQuoteDir: a bare token is returned as-is; a single-quoted token has its
// surrounding quotes stripped and the '"'"' escape sequence collapsed back to a
// literal single quote.
func shellUnquote(s string) string {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return s
	}
	inner := s[1 : len(s)-1]
	return strings.ReplaceAll(inner, `'"'"'`, "'")
}

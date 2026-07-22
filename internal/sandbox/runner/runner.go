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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/registry"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/brokerclient"
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
		// IPv4-only: boid's sandbox is IPv4-only by design (the proxy + DNS
		// forward both bind v4 addresses). Without `-4`, pasta logs
		// "No routable interface for IPv6: IPv6 is disabled" on every launch
		// — pure noise above the harness's own startup output for interactive
		// sessions.
		"-4",
		"-a", "10.0.2.0", "-n", "24", "-g", "10.0.2.2",
		"--dns-forward", "10.0.2.3",
		"-t", "none", "-u", "none",
		"--",
		self, "runner-inner", "--spec", specPath, "--state", statePath,
	}
}

// stopSignal returns the OS signal the runner sets to SIG_IGN. Phase 3-b
// reduced the per-harness StopSignalName to a hard-coded SIGUSR1: claude is
// the only supported harness, and Phase 3-c will revisit this if codex /
// opencode pick a different stop signal. SIG_IGN survives execve so the
// disposition is inherited by pasta and the child runners; the harness
// adapter (claude.Adapter.Run) re-installs signal.Notify on the same signal
// to translate the group signal into a SIGTERM toward the agent process.
func stopSignal() syscall.Signal {
	return syscall.SIGUSR1
}

// ignoreStopSignal sets the harness stop signal to SIG_IGN for the current
// process. SIG_IGN is preserved across execve, so children inherit it.
func ignoreStopSignal(_ sandbox.Spec) {
	signal.Ignore(stopSignal())
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

// --- Backend-shared post-namespace-setup steps ---------------------------
//
// The functions below run after a sandbox's process/mount namespace is
// already in place and its root filesystem already *is* the sandbox root —
// they carry no mount/pivot_root syscalls of their own, so they are equally
// valid called from the userns runner (RunInnerChild, runner_linux.go, once
// pivot_root has landed) and the Phase 6 container entrypoint (RunContainer,
// runner_container_linux.go, where the container runtime's own namespace
// isolation means there is no separate pivot step to wait for at all).
// Extracted per docs/plans/phase6-container-backend.md §PR2 / §決定 2's
// "共有ロジックは runner_linux.go から runner/runner.go に抽出" — see that
// plan section for the shared/not-shared split rationale (mount syscalls
// stay userns-only in runner_linux.go; file/symlink materialization, PATH
// resolution, agent dispatch, and job-done reporting are backend-neutral).

// applySpecFiles writes every spec.FileWrite verbatim at its absolute
// sandbox-internal path, recording an OK/Fail runner-state phase per file
// (and a final "write-files" OK once the whole set has landed — matching
// RunInnerChild's pre-extraction behaviour exactly). stage names the
// runner-state.json stage the phase entries are filed under ("inner-child"
// for the userns runner, "container" for the container entrypoint).
func applySpecFiles(stage string, files []sandbox.FileWrite, st *State) error {
	for _, f := range files {
		if err := writeFileAt(f.Path, f.Content); err != nil {
			st.Fail(stage, "write-file "+f.Path, err)
			return err
		}
	}
	st.OK(stage, "write-files")
	return nil
}

// applySpecSymlinks materializes every spec.Symlink — the Phase 5 5a-3 shim
// materialization path (docs/plans/phase5-shim-and-task-context.md, "5a:
// shim 固定ディレクトリ化" PR3): dispatcher emits `<sandboxShimBinDir>/<name>
// -> boid` for every host command. From Phase 6 on this is also how the
// container entrypoint (re-)creates its own per-container shim symlinks:
// decision 2 forbids baking the per-project `<name>` shims into the shared
// image (unknown at image-build time), so the container entrypoint derives
// them from spec.Symlinks on every container start exactly like the userns
// runner always has.
//
// MkdirAll(parent) is load-bearing here — for the userns runner the parent
// dir (e.g. /run/boid/bin) does not exist yet on the fresh tmpfs root
// pivot_root just placed the process on until the boid-binary mount created
// it (see RunInnerChild's call site); a symlink-only invocation (or the
// container entrypoint, whose image-baked /run/boid/bin already exists but
// may still be missing a deeper parent for a non-default LinkPath) needs the
// same guarantee. Errors are surfaced instead of silently swallowed: a
// silently-dropped shim symlink degrades to command-not-found at runtime
// with no diagnostic pointing back to setup.
func applySpecSymlinks(stage string, symlinks []sandbox.Symlink, st *State) error {
	for _, s := range symlinks {
		if err := os.MkdirAll(filepath.Dir(s.LinkPath), 0o755); err != nil {
			st.Fail(stage, "mkdir symlink parent "+s.LinkPath, err)
			return err
		}
		_ = os.Remove(s.LinkPath)
		if err := os.Symlink(s.LinkTarget, s.LinkPath); err != nil {
			st.Fail(stage, "symlink "+s.LinkPath, err)
			return err
		}
	}
	return nil
}

// applyPathEnv exports spec.Env["PATH"] (when set) as the current process's
// PATH, so the harness adapter's own exec.LookPath / argv resolution sees
// the sandbox PATH rather than whatever launched the runner process.
func applyPathEnv(spec sandbox.Spec) {
	if p := spec.Env["PATH"]; p != "" {
		_ = os.Setenv("PATH", p)
	}
}

// runAgent dispatches every sandbox job (hook / session / exec) through the
// HarnessAdapter pipeline. Phase 3-d retired the legacy runExecArgv branch:
// spec.HarnessType is invariant non-empty here, so the adapter registry
// always returns a concrete implementation. The shell adapter handles
// non-agent hooks and `boid exec`; the claude / codex / opencode adapters
// handle their respective agent jobs.
func runAgent(spec sandbox.Spec) int {
	if spec.HarnessType == "" {
		fmt.Fprintln(os.Stderr, "[boid] runner-inner-child: spec.HarnessType is empty; planner / dispatcher must resolve a harness before dispatch")
		return 127
	}
	adapter := registry.For(spec.HarnessType)
	if adapter == nil {
		fmt.Fprintf(os.Stderr, "[boid] runner-inner-child: unknown harness %q\n", spec.HarnessType)
		return 127
	}

	// Shell adapter consumes Argv / StdinBytes / StdoutCaptureFile from the
	// RunContext directly (it has no CLI conventions to build argv from).
	// Agent adapters ignore those fields and build their own argv per
	// harness convention.
	rc := adapters.RunContext{
		JobID:             spec.ID,
		TaskID:            spec.Env["BOID_TASK_ID"],
		UserAnswer:        spec.UserAnswer,
		InvokedBehavior:   spec.Env["BOID_INVOKED_BEHAVIOR"],
		InvokedName:       spec.Env["BOID_INVOKED_NAME"],
		Model:             spec.Env["BOID_MODEL"],
		Workspace:         spec.WorkDir,
		Env:               spec.Env,
		Argv:              spec.Argv,
		StdinBytes:        spec.StdinBytes,
		StdoutCaptureFile: spec.StdoutCaptureFile,
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
	}

	res, err := adapter.Run(context.Background(), rc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[boid] harness %q run: %v\n", spec.HarnessType, err)
		return 1
	}
	return res.ExitCode
}

// postJobDone resolves the job output (payload patch → stdout capture →
// empty) and posts `boid job done` to the broker, reproducing the former
// EXIT trap. Shared by every backend's entry point that owns a non-
// foreground job's completion report (userns' RunInnerChild, the Phase 6
// container entrypoint's RunContainer). stage names the runner-state.json
// stage the job-done phase entry is filed under.
func postJobDone(stage string, spec sandbox.Spec, exitCode int, st *State) {
	socket := spec.Env["BOID_BROKER_SOCKET"]
	token := spec.Env["BOID_BROKER_TOKEN"]
	if socket == "" {
		// No broker attached (should not happen for non-foreground jobs); the
		// daemon's net will record completion.
		return
	}

	output := resolveJobOutput(spec)
	if err := brokerclient.JobDone(socket, token, spec.ID, spec.WorkDir, exitCode, output); err != nil {
		st.Fail(stage, "job-done", err)
		// Non-fatal: the daemon's "exited without boid job done" net catches it.
		return
	}
	st.OK(stage, "job-done")
}

// resolveJobOutput reproduces buildExitScript's fallback chain: payload patch
// file if present, else the stdout capture file if present, else empty.
func resolveJobOutput(spec sandbox.Spec) []byte {
	if spec.PayloadPatchPath != "" {
		if data, err := os.ReadFile(spec.PayloadPatchPath); err == nil {
			return data
		}
	}
	if spec.StdoutCaptureFile != "" {
		if data, err := os.ReadFile(spec.StdoutCaptureFile); err == nil {
			return data
		}
	}
	return nil
}

func touchIfMissing(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func writeFileAt(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

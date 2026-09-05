// Package runner is the container-backend sandbox runner entry point (`boid
// runner-container`, runner_container_linux.go's RunContainer). See
// docs/{ja,en}/architecture/sandbox-internals.md for the current launch
// chain.
//
// This file holds the portable helpers RunContainer uses (spec decoding,
// signal mapping, the post-namespace-setup file/symlink/PATH/job-done steps)
// so they can be unit-tested independent of any syscall or
// container-runtime path.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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

// stopSignal returns the OS signal the runner sets to SIG_IGN: hard-coded
// SIGUSR1 for every harness. SIG_IGN survives execve so the disposition is
// inherited across it; the harness adapter (claude.Adapter.Run) re-installs
// signal.Notify on the same signal to translate the group signal (relayed
// by the container's PID 1, docker-init/tini) into a SIGTERM toward the
// agent process.
func stopSignal() syscall.Signal {
	return syscall.SIGUSR1
}

// ignoreStopSignal sets the harness stop signal to SIG_IGN for the current
// process. SIG_IGN is preserved across execve, so children inherit it.
func ignoreStopSignal(_ sandbox.Spec) {
	signal.Ignore(stopSignal())
}

// --- Backend-shared post-namespace-setup steps ---------------------------
//
// The functions below run after a sandbox's root filesystem already *is*
// the sandbox root — they carry no mount/pivot_root syscalls of their own.
// RunContainer is the sole caller; kept as free functions here (rather than
// inlined into RunContainer) because it keeps them unit-testable off the
// container-runtime path.

// applySpecFiles writes every spec.FileWrite verbatim at its absolute
// sandbox-internal path, recording an OK/Fail runner-state phase per file
// (and a final "write-files" OK once the whole set has landed). stage names
// the runner-state.json stage the phase entries are filed under ("container"
// for RunContainer, the sole caller).
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

// applySpecSymlinks materializes every spec.Symlink: dispatcher emits
// `<sandboxShimBinDir>/<name> -> boid` for every host command. The
// container entrypoint (re-)creates its own per-container shim symlinks on
// every start rather than baking the per-project `<name>` shims into the
// shared image (unknown at image-build time), so they are always derived
// fresh from spec.Symlinks.
//
// MkdirAll(parent) is load-bearing here — the image-baked /run/boid/bin
// already exists, but may still be missing a deeper parent directory for a
// non-default LinkPath. Errors are surfaced instead of silently swallowed: a
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
// HarnessAdapter pipeline. spec.HarnessType is invariant non-empty here, so
// the adapter registry always returns a concrete implementation. The shell
// adapter handles non-agent hooks and `boid exec`; the claude / codex /
// opencode adapters handle their respective agent jobs.
func runAgent(spec sandbox.Spec) int {
	if spec.HarnessType == "" {
		fmt.Fprintln(os.Stderr, "[boid] runner-container: spec.HarnessType is empty; planner / dispatcher must resolve a harness before dispatch")
		return 127
	}
	adapter := registry.For(spec.HarnessType)
	if adapter == nil {
		fmt.Fprintf(os.Stderr, "[boid] runner-container: unknown harness %q\n", spec.HarnessType)
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

// postJobDone resolves the job output (stdout capture → empty) and posts
// `boid job done` to the broker, reproducing the former EXIT trap. Called
// from RunContainer, the sole entry point for a non-foreground job's
// completion report. stage names the runner-state.json stage the job-done
// phase entry is filed under.
func postJobDone(stage string, spec sandbox.Spec, exitCode int, st *State) {
	token := spec.Env["BOID_BROKER_TOKEN"]
	// JobDone picks UNIX vs TLS from spec.Env itself, mirroring
	// SendJSONFromEnv's own os.Getenv-driven selection for the shim — see
	// brokerclient.JobDone's own doc comment for why spec.Env, not
	// os.Environ(), is the right source here. Neither BOID_BROKER_SOCKET
	// nor BOID_BROKER_TLS_ADDR being set (should not happen for a
	// non-foreground job) is still a no-op return here, not a hard failure:
	// the daemon's own "exited without boid job done" net catches that
	// case regardless.
	if spec.Env["BOID_BROKER_SOCKET"] == "" && spec.Env["BOID_BROKER_TLS_ADDR"] == "" {
		return
	}

	output := resolveJobOutput(spec)
	if err := brokerclient.JobDone(spec.Env, token, spec.ID, spec.WorkDir, exitCode, output); err != nil {
		st.Fail(stage, "job-done", err)
		// Non-fatal: the daemon's "exited without boid job done" net catches it.
		return
	}
	st.OK(stage, "job-done")
}

// resolveJobOutput returns the stdout capture file's content (if any) as the
// job-done output, else empty.
//
// Agents / hook scripts apply their payload patch immediately via the
// broker's `boid task update --payload-patch` RPC — see
// internal/adapters/claude/run.go's sendTaskUpdatePayloadPatch. The
// stdout-capture fallback stays: a shell hook can still report its
// payload_patch by printing `{"payload_patch": ...}` to stdout instead of
// (or as well as) calling the RPC.
func resolveJobOutput(spec sandbox.Spec) []byte {
	if spec.StdoutCaptureFile != "" {
		if data, err := os.ReadFile(spec.StdoutCaptureFile); err == nil {
			return data
		}
	}
	return nil
}

func writeFileAt(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

//go:build linux

package runner

// RunContainer is the `boid runner-container` entry point and the entry
// point of every sandboxed job: the image's own ENTRYPOINT is
// ["/usr/local/bin/boid","runner-container"] (build/container/Dockerfile).
// It runs the post-namespace-setup sequence (write spec.Files, materialize
// spec.Symlinks, run the sandbox-internal clone if declared, resolve PATH,
// dispatch to the harness adapter, report job-done) via the shared helpers
// in runner.go.
//
// The container runtime supplies its own network namespace and egress
// policy (compose network / egress proxy) and already provides
// process/mount namespace isolation — the image's own rootfs *is* the
// sandbox root the moment this process starts, so there is no
// clone/pivot_root sequence to run here. `HostConfig.Init: true`
// (docker-init/tini) is PID 1: it owns zombie reap and SIGUSR1 delivery to
// this process, and the harness adapters' own sigutil.ForwardAndWait
// (internal/adapters/sigutil) translates a received SIGUSR1 into the
// agent's graceful SIGTERM.
func RunContainer(specPath, statePath string) (exitCode int, retErr error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return 1, err
	}
	// SIG_IGN the harness stop signal: the harness adapter (not this
	// process) is the one that reacts to SIGUSR1, via sigutil.ForwardAndWait
	// re-installing signal.Notify on the same signal after execve inherits
	// this disposition.
	ignoreStopSignal(spec)

	st := OpenState(statePath)
	defer st.Close()
	// RunContainer is the sole entry point for its whole run — record the
	// spec dump here so runner-state.json still carries it for diagnosis.
	st.Spec("container", spec)

	// reachedAgent gates the broker job-done: a setup failure (below) sends
	// no `boid job done` and relies on the daemon's "exited without boid
	// job done" net.
	reachedAgent := false
	defer func() {
		if !reachedAgent || spec.Foreground {
			return
		}
		postJobDone("container", spec, exitCode, st)
	}()

	// The workspace HOME's bind-target skeleton, checked before anything
	// else touches the filesystem. It is first on purpose: what it detects
	// is a $HOME the harness cannot write into, and every step below —
	// spec.Files, the clone, the agent itself — would otherwise fail later,
	// further from the cause, or not at all. See verifyHomeSkeleton for why
	// this check lives in the job container rather than on the daemon.
	if err := verifyHomeSkeleton(spec); err != nil {
		st.Fail("container", "home-skeleton", err)
		return 1, err
	}

	// spec.Files: the container's own root filesystem already *is* the
	// sandbox root.
	if err := applySpecFiles("container", spec.Files, st); err != nil {
		return 1, err
	}

	// spec.Symlinks: per-project host-command shims, re-derived fresh from
	// the validated spec on every container start rather than baked into
	// the shared image — see applySpecSymlinks' doc comment (runner.go).
	if err := applySpecSymlinks("container", spec.Symlinks, st); err != nil {
		return 1, err
	}

	// Sandbox-internal clone + branch resolution. No-op unless
	// spec.Clone.Enabled.
	if spec.Clone.Enabled {
		if err := performClone(spec.Clone, st); err != nil {
			st.Fail("container", "clone", err)
			return 1, err
		}
		st.OK("container", "clone")
	}

	// Resolve the agent argv against the sandbox PATH.
	applyPathEnv(spec)

	reachedAgent = true
	st.OK("container", "run-agent")
	exitCode = runAgent(spec)
	return exitCode, nil
}

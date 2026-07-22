//go:build linux

package runner

// RunContainer is the `boid runner-container` entry point
// (docs/plans/phase6-container-backend.md §PR2 / §決定 2/3). It is the
// container-backend counterpart to RunInnerChild (runner_linux.go): both run
// the same post-namespace-setup sequence (write spec.Files, materialize
// spec.Symlinks, run the sandbox-internal clone if declared, resolve PATH,
// dispatch to the harness adapter, report job-done) via the shared helpers
// in runner.go. What RunContainer does *not* do, by design, is everything
// userns-specific that RunOuter/RunInner/RunInnerChild otherwise chain
// through:
//
//   - No pasta relay (RunOuter/RunInner) — the container runtime supplies
//     its own network namespace and egress policy (compose network / egress
//     proxy, decision 5); there is no inner pasta hop to launch.
//   - No clone(CLONE_NEWUSER|CLONE_NEWNS) / mount syscalls / pivot_root
//     (setupMountNamespace/pivotInto) — the container runtime already
//     provides the process/mount namespace isolation and the image's own
//     rootfs *is* the sandbox root the moment this process starts (決定 2:
//     "container entrypoint は clone/pivot_root を skip … namespace 隔離は
//     container が提供").
//   - No self-authored signal-forwarding loop or PID 1 reap loop. Decision 3
//     pins `HostConfig.Init: true` (docker-init/tini) as PID 1: it owns
//     zombie reap and SIGUSR1 delivery to this process, and the harness
//     adapters' own sigutil.ForwardAndWait (internal/adapters/sigutil)
//     already translates a received SIGUSR1 into the agent's graceful
//     SIGTERM — exactly the same as it does today when this same code runs
//     as the userns runner's L3 child. Nothing new to write here.
//
// RunContainer is still entirely inert as of PR2: no dispatcher /
// containerBackend code path invokes `boid runner-container` yet — that
// wiring lands in PR5 (config still non-public until the PR7 cutover).
func RunContainer(specPath, statePath string) (exitCode int, retErr error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return 1, err
	}
	// SIG_IGN the harness stop signal for the same reason RunInnerChild
	// does: the harness adapter (not this process) is the one that reacts
	// to SIGUSR1, via sigutil.ForwardAndWait re-installing signal.Notify on
	// the same signal after execve inherits this disposition.
	ignoreStopSignal(spec)

	st := OpenState(statePath)
	defer st.Close()
	// Unlike the userns chain (where only RunOuter records the launch-time
	// spec, once, before handing off to pasta), RunContainer is the sole
	// entry point for its whole run — record the spec dump here so
	// runner-state.json still carries it for diagnosis. pastaCmdline is nil:
	// there is no pasta hop in the container path.
	st.Spec("container", spec, nil)

	// reachedAgent gates the broker job-done, mirroring RunInnerChild: a
	// setup failure (below) sends no `boid job done` and relies on the
	// daemon's "exited without boid job done" net.
	reachedAgent := false
	defer func() {
		if !reachedAgent || spec.Foreground {
			return
		}
		postJobDone("container", spec, exitCode, st)
	}()

	// spec.Files: the container's own root filesystem already *is* the
	// sandbox root (no pivot_root to wait for, unlike RunInnerChild).
	if err := applySpecFiles("container", spec.Files, st); err != nil {
		return 1, err
	}

	// spec.Symlinks: per-project host-command shims. Decision 2 forbids
	// baking these into the shared image (unknown at image-build time), so
	// every container start re-derives them fresh from the validated spec —
	// see applySpecSymlinks' doc comment (runner.go).
	if err := applySpecSymlinks("container", spec.Symlinks, st); err != nil {
		return 1, err
	}

	// Sandbox-internal clone + branch resolution
	// (docs/plans/git-gateway-cutover.md PR5). No-op unless
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

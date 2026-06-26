//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/registry"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/brokerclient"
	"golang.org/x/sys/unix"
)

// RunOuter is the `boid runner-outer` entry point. It runs on the host as the
// daemon's direct child and manages the pasta lifecycle: it launches
// `pasta … -- boid runner-inner`, captures pasta's own stderr to a temp file
// (dumped only on failure), then performs host-side cleanup of the sandbox ROOT
// and staging dirs after pasta returns. Mirrors the former outer.sh.
func RunOuter(specPath, statePath string) (int, error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return 1, err
	}
	// SIG_IGN the harness stop signal so a process-group stop signal does not
	// kill this host process; the disposition is inherited by pasta and the
	// child runners (see ignoreStopSignal).
	ignoreStopSignal(spec)

	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("resolve boid binary: %w", err)
	}
	args := pastaArgs(self, specPath, statePath)

	st := OpenState(statePath)
	defer st.Close()
	st.Spec("outer", spec, append([]string{"pasta"}, args...))

	cmd := exec.Command("pasta", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout

	// stderr routing, reproducing outer.sh:
	//   - non-TTY: pasta + the sandbox command share fd 2 → a temp file. pasta's
	//     diagnostics (and the agent's stderr) are kept off the daemon transcript
	//     and dumped only on failure, matching outer.sh's mktemp + `cat on error`.
	//   - TTY: the agent owns the PTY, so pasta inherits the PTY on stderr. pasta
	//     is silent on success; on failure its diagnostics surface on the terminal
	//     (acceptable — the session is failing anyway). This avoids the fragile
	//     fd-passing dance the bash scripts used to separate the two streams.
	var pastaErrPath string
	if spec.TTY {
		cmd.Stderr = os.Stderr
	} else {
		pastaErr, err := os.CreateTemp("", "boid-pasta-stderr-*.log")
		if err != nil {
			return 1, fmt.Errorf("create pasta stderr file: %w", err)
		}
		pastaErrPath = pastaErr.Name()
		cmd.Stderr = pastaErr
		defer pastaErr.Close()
	}

	st.OK("outer", "pasta-start")
	runErr := cmd.Run()
	exitCode := commandExitCode(runErr)

	if pastaErrPath != "" && exitCode != 0 {
		if info, statErr := os.Stat(pastaErrPath); statErr == nil && info.Size() > 0 {
			fmt.Fprintf(os.Stderr, "[boid] pasta stderr (exit_code=%d):\n", exitCode)
			if data, readErr := os.ReadFile(pastaErrPath); readErr == nil {
				_, _ = os.Stderr.Write(data)
			}
		}
	}
	if exitCode != 0 {
		st.Fail("outer", "pasta-exit", fmt.Errorf("exit_code=%d", exitCode))
	} else {
		st.OK("outer", "pasta-exit")
	}
	if pastaErrPath != "" {
		_ = os.Remove(pastaErrPath)
	}

	// Host-side cleanup (the sandbox mount namespace is already gone, so binds
	// inside it cannot cause rm to traverse onto host files).
	cleanupRoot(spec.RootDir)
	for _, p := range spec.CleanupPaths {
		_ = os.RemoveAll(p)
	}
	// The spec file carries secrets (broker token, API keys): remove it
	// unconditionally. The state file is retained on failure for diagnosis.
	_ = os.Remove(specPath)
	if exitCode == 0 {
		_ = os.Remove(statePath)
	}

	return exitCode, nil
}

// cleanupRoot removes the sandbox ROOT directory, guarded by the
// /tmp/boid-root- prefix so a misconfigured RootDir is never rm -rf'd. Matches
// outer.sh's `case "$root_dir" in /tmp/boid-root-*) …` guard.
func cleanupRoot(rootDir string) {
	switch {
	case rootDir == "":
		return
	case strings.HasPrefix(rootDir, "/tmp/boid-root-"):
		_ = os.RemoveAll(rootDir)
	default:
		fmt.Fprintf(os.Stderr, "[boid] WARNING: root_dir=%s not under /tmp/boid-root-*, skipping cleanup\n", rootDir)
	}
}

// RunInner is the `boid runner-inner` entry point. It runs inside pasta's
// user+net namespace (inner uid 0). It applies the nft egress rules, then clones
// runner-inner-child into a fresh user+mount namespace. Mirrors the former
// setup.sh's nft + `unshare --user … --root` hand-off.
func RunInner(specPath, statePath string) (int, error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return 1, err
	}
	ignoreStopSignal(spec)

	st := OpenState(statePath)
	defer st.Close()

	// nft egress filtering — applied here while we hold uid 0 / CAP_NET_ADMIN in
	// pasta's network namespace. Any failure aborts the sandbox (no rollback).
	plan := sandbox.BuildPlan(spec)
	for _, rule := range plan.NFTRules {
		phase := "nft " + strings.Join(rule.Args, " ")
		c := exec.Command("nft", rule.Args...)
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			st.Fail("inner", phase, err)
			return 1, fmt.Errorf("apply nft rule %v: %w", rule.Args, err)
		}
		st.OK("inner", phase)
	}

	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("resolve boid binary: %w", err)
	}

	child := exec.Command(self, "runner-inner-child", "--spec", specPath, "--state", statePath)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = os.Environ()
	// Create the L3 user+mount namespace via clone flags on the child (pitfall:
	// unix.Unshare(CLONE_NEWUSER) on a running goroutine EINVALs because the Go
	// runtime is multi-threaded; SysProcAttr.Cloneflags does it correctly at
	// fork). uid_map: host uid 1000 → inner uid 0. Inner uid 0 is essential —
	// only the owner of a user ns holds CAP_SYS_ADMIN over the mount ns it owns,
	// so MS_PRIVATE / bind / pivot_root all succeed inside L3. Mapping to inner
	// uid 1000 (the earlier mistake on commit 89e1307) loses CAP_SYS_ADMIN and
	// MS_PRIVATE on / EPERMs. setgroups is denied per the unprivileged user-ns
	// safety requirement.
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		GidMappingsEnableSetgroups: false,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Geteuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getegid(), Size: 1},
		},
	}

	st.OK("inner", "clone-child")
	runErr := child.Run()
	exitCode := commandExitCode(runErr)
	if exitCode != 0 {
		st.Fail("inner", "child-exit", fmt.Errorf("exit_code=%d", exitCode))
	} else {
		st.OK("inner", "child-exit")
	}
	return exitCode, nil
}

// RunInnerChild is the `boid runner-inner-child` entry point (L3). It runs in
// the cloned user+mount namespace, lays out the sandbox root via bind mounts,
// pivot_root's into it, writes the context files, runs the agent, and posts the
// broker job-done. Mirrors the former inner.sh.
func RunInnerChild(specPath, statePath string) (exitCode int, retErr error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return 1, err
	}
	ignoreStopSignal(spec)

	st := OpenState(statePath)
	defer st.Close()

	// reachedAgent gates the broker job-done: the former inner.sh installed its
	// EXIT trap only just before running argv, so a setup failure (mounts /
	// pivot) sent no `boid job done` and relied on the daemon's "exited without
	// boid job done" net. Reproduce that: only post job-done once we are about
	// to (or did) run the agent.
	reachedAgent := false
	defer func() {
		if !reachedAgent || spec.Foreground {
			return
		}
		postJobDone(spec, exitCode, st)
	}()

	root := spec.RootDir
	if root == "" {
		return 1, errors.New("runner-inner-child: spec.RootDir is empty")
	}

	if err := setupMountNamespace(spec, root, st); err != nil {
		st.Fail("inner-child", "mount-setup", err)
		return 1, err
	}

	if err := pivotInto(root, spec.Profile == sandbox.ProfileInit); err != nil {
		st.Fail("inner-child", "pivot-root", err)
		return 1, err
	}
	st.OK("inner-child", "pivot-root")

	// Context files live under the now-mounted tmpfs HOME, so they must be
	// written after pivot_root (otherwise the HOME tmpfs would shadow them).
	for _, f := range spec.Files {
		if err := writeFileAt(f.Path, f.Content); err != nil {
			st.Fail("inner-child", "write-file "+f.Path, err)
			return 1, err
		}
	}
	st.OK("inner-child", "write-context-files")

	for _, s := range spec.Symlinks {
		_ = os.Remove(s.LinkPath)
		_ = os.Symlink(s.LinkTarget, s.LinkPath)
	}

	// Resolve the agent argv against the sandbox PATH.
	if p := spec.Env["PATH"]; p != "" {
		_ = os.Setenv("PATH", p)
	}

	reachedAgent = true
	st.OK("inner-child", "run-agent")
	exitCode = runAgent(spec)
	return exitCode, nil
}

// setupMountNamespace composes the sandbox root filesystem inside the cloned
// mount namespace: make all mounts private, mount a tmpfs as the new root,
// apply the base + caller mounts, and write the plan files (DNS) under ROOT.
func setupMountNamespace(spec sandbox.Spec, root string, st *State) error {
	// 1) Detach mount propagation so binds don't leak to the parent ns and
	// pivot_root is permitted (MS_SHARED on / would EINVAL).
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	st.OK("inner-child", "ms-private")

	// 2) New root as its own tmpfs mount (pivot_root requires new_root to be a
	// mount point distinct from the old root).
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("mkdir root: %w", err)
	}
	if err := unix.Mount("tmpfs", root, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount tmpfs root: %w", err)
	}
	st.OK("inner-child", "tmpfs-root")

	plan := sandbox.BuildPlan(spec)
	for _, m := range plan.Mounts {
		if err := applyMount(root, m); err != nil {
			return fmt.Errorf("mount %s: %w", m.Target, err)
		}
	}
	st.OK("inner-child", "mounts")

	// Plan files (DNS stub) are written under ROOT before pivot.
	for _, f := range plan.Files {
		if err := writeFileAt(filepath.Join(root, f.Path), f.Content); err != nil {
			return fmt.Errorf("write plan file %s: %w", f.Path, err)
		}
	}
	return nil
}

// applyMount materializes a single Mount under root, reproducing the target
// creation + mount + post-op (rslave / needsdirs / ro-remount) sequence the
// former render.go emitted as shell.
func applyMount(root string, m sandbox.Mount) error {
	if !evalGuard(m.Guard) {
		return nil
	}
	target := root + m.Target

	switch {
	case m.DetectType:
		info, err := os.Stat(m.Source)
		switch {
		case err == nil && info.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case err == nil:
			// file / socket / fifo: ensure parent dir + an empty mountpoint file.
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := touchIfMissing(target); err != nil {
				return err
			}
		default:
			// Source missing and no guard caught it: nothing to create; the
			// mount below will surface the error.
		}
	case m.IsFile:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := touchIfMissing(target); err != nil {
			return err
		}
	default:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
	}

	switch m.Type {
	case sandbox.MountBind:
		if err := unix.Mount(m.Source, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s→%s: %w", m.Source, target, err)
		}
	case sandbox.MountRBind:
		if err := unix.Mount(m.Source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s→%s: %w", m.Source, target, err)
		}
	case sandbox.MountTmpfs:
		if err := unix.Mount("tmpfs", target, "tmpfs", 0, ""); err != nil {
			return fmt.Errorf("tmpfs %s: %w", target, err)
		}
	}

	if m.Slave {
		if err := unix.Mount("", target, "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("make-rslave %s: %w", target, err)
		}
	}
	for _, d := range m.NeedsDirs {
		if err := os.MkdirAll(filepath.Join(target, d), 0o755); err != nil {
			return err
		}
	}
	if m.ReadOnly {
		if err := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("ro-remount %s: %w", target, err)
		}
	}
	return nil
}

// pivotInto changes the process root to root.
//
// For ProfileInit (hasHostRootRBind == true) the plan mounts the entire host
// root as a read-only rbind ON TOP of the tmpfs at root. This makes root
// appear as the host filesystem (ro), so pivot_root's put_old MkdirAll would
// hit EROFS. We use chroot instead: it requires only CAP_SYS_CHROOT (held via
// user-namespace uid 0 mapping) and is sufficient for ProfileInit because the
// host filesystem is intentionally accessible — security isolation comes from
// the mount namespace and the writable-path allowlist, not from detaching the
// old root.
//
// For all other profiles we use pivot_root which fully detaches the old root.
func pivotInto(root string, hasHostRootRBind bool) error {
	if hasHostRootRBind {
		// chroot path for ProfileInit.
		if err := os.Chdir(root); err != nil {
			return fmt.Errorf("chdir to root: %w", err)
		}
		if err := unix.Chroot(root); err != nil {
			return fmt.Errorf("chroot: %w", err)
		}
		if err := os.Chdir("/"); err != nil {
			return fmt.Errorf("chdir / after chroot: %w", err)
		}
		return nil
	}

	// pivot_root path for all other profiles.
	oldRoot := filepath.Join(root, ".oldroot")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir .oldroot: %w", err)
	}
	if err := unix.PivotRoot(root, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	if err := unix.Unmount("/.oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Remove("/.oldroot"); err != nil {
		return fmt.Errorf("remove .oldroot: %w", err)
	}
	return nil
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

// postJobDone resolves the job output (payload patch → stdout capture → empty)
// and posts `boid job done` to the broker, reproducing the former EXIT trap.
func postJobDone(spec sandbox.Spec, exitCode int, st *State) {
	socket := spec.Env["BOID_BROKER_SOCKET"]
	token := spec.Env["BOID_BROKER_TOKEN"]
	if socket == "" {
		// No broker attached (should not happen for non-foreground jobs); the
		// daemon's net will record completion.
		return
	}

	output := resolveJobOutput(spec)
	if err := brokerclient.JobDone(socket, token, spec.ID, spec.WorkDir, exitCode, output); err != nil {
		st.Fail("inner-child", "job-done", err)
		// Non-fatal: the daemon's "exited without boid job done" net catches it.
		return
	}
	st.OK("inner-child", "job-done")
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

// commandExitCode extracts the exit code from an exec.Cmd error, mirroring the
// dispatcher's exitCode helper (signalled → 128+signal).
func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	// Start failure (e.g. binary not found): mirror bash's 127.
	if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
		return 127
	}
	return 1
}

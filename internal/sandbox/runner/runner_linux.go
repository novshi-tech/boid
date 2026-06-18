//go:build linux

package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/brokerclient"
	"golang.org/x/sys/unix"
)

// The go-native sandbox launch chain mirrors the former bash trio. Namespaces
// must be created at fork (SysProcAttr.Cloneflags) — never via unix.Unshare on a
// running goroutine, which EINVALs/EPERMs under Go's multi-threaded runtime — so
// each namespace transition is its own re-exec of this binary:
//
//	daemon → bash -lc → runner-outer → pasta → runner-inner → runner-mount → runner-inner-child → agent
//	                    (host)         (netns)  (L2 uid 0)     (L2 mountns)   (L3 uid 1000, chroot)
//
//   - runner-inner: applies nft (in pasta's net ns, uid 0), then clones the
//     mount-ns child with CLONE_NEWNS *only*. Doing the mounts in a CLONE_NEWNS
//     ns that stays in pasta's user namespace is essential: the new mount ns is
//     then owned by a user ns we hold CAP_SYS_ADMIN in, so MS_PRIVATE / bind /
//     etc. succeed. (Combining NEWUSER|NEWNS in one clone makes "/" owned by the
//     parent user ns → MS_PRIVATE EPERMs.) This reproduces `unshare --mount`.
//   - runner-mount: lays out the sandbox root via bind mounts, then clones the
//     agent host with CLONE_NEWUSER + uid map 1000←0 + Chroot=ROOT. chroot (not
//     pivot_root) is used because pivot_root needs CAP_SYS_ADMIN over the mount
//     ns's owning user ns, which the L3 user ns does not have; chroot only needs
//     CAP_SYS_CHROOT in L3. This reproduces `unshare --user --map-user=1000 --root`.
//   - runner-inner-child: chrooted as uid 1000. It cannot read the host
//     /tmp/...-runner-spec.json (its /tmp is the sandbox tmpfs), so runner-mount
//     passes the spec and runner-state files as inherited fds (ExtraFiles → fd
//     3/4). It writes the context files, runs the agent, and posts broker job-done.

// fd numbers runner-mount passes to runner-inner-child via ExtraFiles.
const (
	innerChildSpecFD  = 3
	innerChildStateFD = 4
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
// runner-mount into a fresh mount namespace (CLONE_NEWNS only, staying in this
// user namespace). Mirrors setup.sh's nft step.
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
	child := exec.Command(self, "runner-mount", "--spec", specPath, "--state", statePath)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = os.Environ()
	// CLONE_NEWNS only: the new mount ns is owned by pasta's user ns, where we
	// hold CAP_SYS_ADMIN, so the mounts (incl. MS_PRIVATE) succeed.
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}

	st.OK("inner", "clone-mountns")
	exitCode := commandExitCode(child.Run())
	if exitCode != 0 {
		st.Fail("inner", "mount-exit", fmt.Errorf("exit_code=%d", exitCode))
	} else {
		st.OK("inner", "mount-exit")
	}
	return exitCode, nil
}

// RunMount is the `boid runner-mount` entry point. It runs in the fresh mount
// namespace (still pasta's user ns, uid 0) and lays out the sandbox root via
// bind mounts, then clones runner-inner-child into a new user ns (uid 1000) and
// chroots it into ROOT. Mirrors setup.sh's mount work + `unshare --user --root`.
func RunMount(specPath, statePath string) (int, error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return 1, err
	}
	ignoreStopSignal(spec)

	st := OpenState(statePath)
	defer st.Close()

	root := spec.RootDir
	if root == "" {
		return 1, errors.New("runner-mount: spec.RootDir is empty")
	}
	if err := setupMounts(spec, root, st); err != nil {
		st.Fail("mount", "mount-setup", err)
		return 1, err
	}

	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("resolve boid binary: %w", err)
	}

	// The chrooted child cannot read the host spec/state by path (its /tmp is the
	// sandbox tmpfs), so hand them over as inherited fds.
	specF, err := os.Open(specPath)
	if err != nil {
		return 1, fmt.Errorf("open spec for child: %w", err)
	}
	defer specF.Close()

	child := exec.Command(self, "runner-inner-child")
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = os.Environ()
	child.Dir = spec.WorkDir // chdir (after chroot) into the project/worktree
	child.ExtraFiles = []*os.File{specF}
	if st != nil && st.f != nil {
		child.ExtraFiles = append(child.ExtraFiles, st.f) // fd 4 = runner-state
	}
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		GidMappingsEnableSetgroups: false,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 1000, HostID: os.Geteuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 1000, HostID: os.Getegid(), Size: 1}},
		Chroot:                     root,
	}

	st.OK("mount", "clone-userns-chroot")
	exitCode := commandExitCode(child.Run())
	if exitCode != 0 {
		st.Fail("mount", "child-exit", fmt.Errorf("exit_code=%d", exitCode))
	} else {
		st.OK("mount", "child-exit")
	}
	return exitCode, nil
}

// RunInnerChild is the `boid runner-inner-child` entry point (L3). It is already
// chrooted into ROOT and mapped to uid 1000 by runner-mount's clone. It reads
// the spec / runner-state from inherited fds (3 / 4), writes the context files,
// runs the agent, and posts the broker job-done. Mirrors inner.sh.
func RunInnerChild() (exitCode int, retErr error) {
	spec, err := readSpecFromFD(innerChildSpecFD)
	if err != nil {
		return 1, err
	}
	ignoreStopSignal(spec)

	st := OpenStateFromFD(innerChildStateFD)
	defer st.Close()

	// reachedAgent gates the broker job-done: inner.sh installed its EXIT trap
	// only just before running argv, so a setup failure sent no `boid job done`
	// and relied on the daemon's "exited without boid job done" net. Reproduce
	// that: only post job-done once we are about to (or did) run the agent.
	reachedAgent := false
	defer func() {
		if !reachedAgent || spec.Foreground {
			return
		}
		postJobDone(spec, exitCode, st)
	}()

	// Context files live under the tmpfs HOME (mounted by runner-mount); we are
	// chrooted, so absolute paths resolve inside the sandbox root.
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

// setupMounts composes the sandbox root filesystem under root: make all mounts
// private (so binds don't leak to the parent ns), apply the base + caller
// mounts into root's subtree, and write the plan files (DNS). root stays a plain
// directory (chroot, not pivot_root, enters it), mirroring setup.sh.
func setupMounts(spec sandbox.Spec, root string, st *State) error {
	// `unshare --mount` defaulted to recursive-private propagation; reproduce it
	// so the binds below do not propagate back to the host mount namespace.
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	st.OK("mount", "ms-private")

	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("mkdir root: %w", err)
	}

	plan := sandbox.BuildPlan(spec)
	for _, m := range plan.Mounts {
		if err := applyMount(root, m); err != nil {
			return fmt.Errorf("mount %s: %w", m.Target, err)
		}
	}
	st.OK("mount", "mounts")

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

// runAgent fork-execs the agent argv, wiring stdio per the same precedence the
// former inner.sh used, and returns its exit code. cwd is already spec.WorkDir
// (set by runner-mount via cmd.Dir after chroot).
func runAgent(spec sandbox.Spec) int {
	if len(spec.Argv) == 0 {
		fmt.Fprintln(os.Stderr, "[boid] runner-inner-child: empty argv")
		return 127
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = envSlice(spec.Env)
	cmd.Dir = spec.WorkDir

	var captureFile *os.File
	switch {
	case len(spec.StdinBytes) > 0 && spec.StdoutCaptureFile != "":
		cmd.Stdin = bytes.NewReader(spec.StdinBytes)
		f, err := os.Create(spec.StdoutCaptureFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[boid] create stdout capture: %v\n", err)
			return 127
		}
		captureFile = f
		cmd.Stdout = f
		cmd.Stderr = os.Stderr
	case len(spec.StdinBytes) > 0:
		cmd.Stdin = bytes.NewReader(spec.StdinBytes)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	default:
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	err := cmd.Run()
	if captureFile != nil {
		_ = captureFile.Close()
	}
	return commandExitCode(err)
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

// readSpecFromFD reads and decodes the sandbox spec from an inherited fd.
func readSpecFromFD(fd int) (sandbox.Spec, error) {
	var spec sandbox.Spec
	f := os.NewFile(uintptr(fd), "runner-spec")
	if f == nil {
		return spec, fmt.Errorf("runner spec fd %d not available", fd)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return spec, fmt.Errorf("read runner spec fd %d: %w", fd, err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("decode runner spec fd %d: %w", fd, err)
	}
	return spec, nil
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

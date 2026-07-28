package dispatcher_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// statefulBackend is a minimal backend.SandboxBackend recording every
// Launch call's opts, keyed by the generated runtimeID, so Dispatch-level
// tests can assert on what a real launch would have received
// (LaunchOpts) without a real container/userns runtime.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) replaced this file's
// former statefulRuntime (a JobRuntime fake wrapping Runner.Sandbox +
// Runner.Runtime through the removed userns backend's fallback
// construction) — Runner.Backend is now the sole launch seam, so the fake
// implements backend.SandboxBackend directly instead.
type statefulBackend struct {
	mu       sync.Mutex
	nextID   int
	sessions map[string]*statefulSession
	launches map[string]backend.LaunchOptions
}

var _ backend.SandboxBackend = (*statefulBackend)(nil)

func newStatefulBackend() *statefulBackend {
	return &statefulBackend{
		sessions: make(map[string]*statefulSession),
		launches: make(map[string]backend.LaunchOptions),
	}
}

func (b *statefulBackend) Launch(_ context.Context, _ sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := opts.DesiredID
	if id == "" {
		id = fmt.Sprintf("runtime-%d", b.nextID)
		b.nextID++
	}
	sess := &statefulSession{id: id, owner: b}
	b.sessions[id] = sess
	b.launches[id] = opts
	return sess, nil
}

func (b *statefulBackend) Adopt(_ context.Context, runtimeID string) (backend.SandboxSession, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[runtimeID]
	return sess, ok
}

// EnsureWorkspaceHomeVolume and RunWorkspaceInit satisfy
// dispatcher.WorkspaceInitExecutor, which resolveWorkspaceHome requires of
// every backend since PR5/PR6 — see runWorkspaceInitInProcess
// (runtime_test_helpers_test.go).
func (b *statefulBackend) EnsureWorkspaceHomeVolume(_ context.Context, req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	return ensureWorkspaceHomeVolumeInProcess(req)
}

func (b *statefulBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	return runWorkspaceInitInProcess(ctx, req)
}

func (b *statefulBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// ActiveRuntimeIDs returns the sorted set of runtimeIDs currently tracked
// (launched, not yet stopped-and-removed).
func (b *statefulBackend) ActiveRuntimeIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.sessions))
	for id := range b.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// LaunchOpts returns the backend.LaunchOptions a previous Launch call
// recorded for runtimeID — the successor to the pre-PR-4 StartSpec, now
// returning LaunchOptions (the seam Launch itself receives) rather than the
// removed dispatcher.RuntimeStartSpec.
func (b *statefulBackend) LaunchOpts(runtimeID string) (backend.LaunchOptions, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	opts, ok := b.launches[runtimeID]
	return opts, ok
}

// statefulSession is a minimal backend.SandboxSession: Stop removes itself
// from the owning backend's session map (mirroring the pre-PR-4
// statefulRuntime.Stop's bookkeeping), everything else is a no-op.
type statefulSession struct {
	id string

	mu      sync.Mutex
	stopped bool
	owner   *statefulBackend
}

var _ backend.SandboxSession = (*statefulSession)(nil)

func (s *statefulSession) ID() string { return s.id }
func (s *statefulSession) Subscribe() ([]byte, <-chan []byte, func(), bool, bool) {
	return nil, nil, func() {}, false, true
}
func (s *statefulSession) WriteInput([]byte) error           { return nil }
func (s *statefulSession) CloseInput() error                 { return nil }
func (s *statefulSession) Resize(backend.TerminalSize) error { return nil }

// Wait returns immediately with ErrRuntimeUnsupported — mirroring the
// pre-PR-4 statefulRuntime.Wait's exact contract (it never actually
// blocked; Runner never calls Wait on this fake in production dispatch
// paths, and Runner.cleanupSandboxAfterWait's own ErrRuntimeUnsupported
// branch treats this as "runtime unsupported, reap and move on" rather
// than an error to surface). Returning immediately (not blocking on
// ctx.Done, which is never cancelled by these tests) avoids leaking the
// launchSandbox-spawned cleanup goroutine for the life of the test binary.
func (s *statefulSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, dispatcher.ErrRuntimeUnsupported
}

func (s *statefulSession) Stop(context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	if s.owner != nil {
		s.owner.mu.Lock()
		delete(s.owner.sessions, s.id)
		s.owner.mu.Unlock()
	}
	return nil
}

func (s *statefulSession) Signal(context.Context, syscall.Signal) error { return nil }

// The two functions below are this package's (dispatcher_test, the EXTERNAL
// test package) stand-in for what a real backend does for a workspace home:
// create the named volume that holds it (PR6), and run the preparation wrapper
// against that volume (PR5).
//
// Every fake backend here embeds them rather than returning nil, because
// resolveWorkspaceHome's contract depends on the run having HAPPENED: the
// bind-target skeleton has to exist, or the very next dispatch's job container
// gets a $HOME whose ~/.claude the ENGINE created as uid 0. A no-op would leave
// every Dispatch test running against a workspace home in a state production
// never produces.
//
// A docker volume is modelled as a directory and its identity label as a
// sibling file. Modelling the identity rather than returning a constant is what
// keeps these fakes honest about the property the fast path rests on:
// VolumeCreate reports the EXISTING volume's label, so a second dispatch sees
// the same identity and does not re-run init.

// workspaceHomeVolumeDirForTests roots the modelled volumes under whatever
// throwaway XDG_DATA_HOME testutil/homeenv established, so they are removed
// with it and no two tests share one.
func workspaceHomeVolumeDirForTests(name string) string {
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "boid-test-volumes", name)
}

func ensureWorkspaceHomeVolumeInProcess(req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	dir := workspaceHomeVolumeDirForTests(req.Name)
	idPath := dir + ".id"
	if data, err := os.ReadFile(idPath); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(idPath, []byte(req.CandidateID), 0o600); err != nil {
		return "", err
	}
	return req.CandidateID, nil
}

// runWorkspaceInitInProcess runs the wrapper resolveWorkspaceHome assembled
// under a real bash, in this test process. It is not a container: there is no
// mount, so HOME/BOID_WORKSPACE_HOME are rewritten from the request's
// container-side target to the directory modelling its volume.
func runWorkspaceInitInProcess(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	homeDir := workspaceHomeVolumeDirForTests(req.HomeSource)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return err
	}
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = homeDir
		}
		env = append(env, k+"="+v)
	}
	env = append(env, "PATH="+os.Getenv("PATH"))

	cmd := exec.CommandContext(ctx, "/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("workspace init (in-process stand-in) failed: %w\n%s", err, out)
	}
	return nil
}

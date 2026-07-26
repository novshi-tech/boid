package dispatcher_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// RunWorkspaceInit satisfies dispatcher.WorkspaceInitExecutor, which
// resolveWorkspaceHome requires of every backend since PR5 — see
// runWorkspaceInitInProcess (runtime_test_helpers_test.go).
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
func (s *statefulSession) Subscribe() ([]byte, <-chan []byte, func(), bool) {
	return nil, nil, func() {}, false
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

// runWorkspaceInitInProcess is this package's stand-in for the throwaway init
// container PR5 introduced (docs/plans/workspace-home-volume-persistence.md
// 論点 c): it runs the wrapper resolveWorkspaceHome assembled under a real
// bash, in this test process.
//
// Every fake backend in this package embeds it rather than returning nil,
// because resolveWorkspaceHome's contract depends on the run having HAPPENED:
// the bind-target skeleton has to exist and the home has to carry its identity
// token, or the very next dispatch re-runs init and logs a desync warning. A
// no-op would leave every Dispatch test running against a workspace home in a
// state production never produces.
//
// It is not a container. HOME/BOID_WORKSPACE_HOME are rewritten from the
// request's container-side target back to its host-side source, since there is
// no mount here to make the two the same thing.
func runWorkspaceInitInProcess(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = req.HomeSource
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

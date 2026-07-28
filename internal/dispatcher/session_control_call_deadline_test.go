package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins next-session-container-backend-loose-ends.md §2: every
// foreground SandboxSession control call (Resize / Stop / Signal / the
// Adopt lookup that precedes them) must be bounded, so a wedged
// docker/podman engine socket that accepts a request and never answers
// costs the caller a bounded delay and an error, not a permanent hang.
// session.Wait is deliberately NOT covered here — it blocks for the job's
// entire lifetime by design and must keep an unbounded context (see
// session.Wait's own doc comment).

// withSessionControlCallTimeout shrinks sessionControlCallTimeout for the
// duration of one test — this package's established pattern for pinning a
// timeout without a real test actually waiting out the production value
// (see e.g. withSelfInspectTimeout in daemon_state_volume_test.go).
func withSessionControlCallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sessionControlCallTimeout.Set(d)
	t.Cleanup(func() { sessionControlCallTimeout.Set(prev) })
}

// --- containerSession.Resize --------------------------------------------

// TestContainerSession_Resize_HangingEngineHitsDeadline pins the
// container_backend.go:2409 fix: Resize's backend.SandboxSession interface
// method takes no context to inherit a deadline from, so it must synthesize
// its own bounded one rather than calling ContainerResize under a bare
// context.Background().
func TestContainerSession_Resize_HangingEngineHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	api := &fakeDockerAPI{
		ContainerResizeFunc: func(ctx context.Context, containerID string, options client.ContainerResizeOptions) (client.ContainerResizeResult, error) {
			<-ctx.Done()
			return client.ContainerResizeResult{}, ctx.Err()
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-resize", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-resize"})

	start := time.Now()
	err := sess.Resize(backend.TerminalSize{Rows: 40, Cols: 100})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Resize took %v against an engine that never answers; a WS resize frame would hang the connection", elapsed)
	}
	if err == nil {
		t.Fatal("want an error when the engine never answers")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

// --- Runner.StopJobRuntime / Runner.SignalJobRuntime ---------------------

// hangingControlSession is a backend.SandboxSession whose Stop/Signal block
// until the context they are given is done — modelling a wedged
// docker/podman engine socket that accepts the request and never answers.
// Resize/Subscribe/WriteInput/CloseInput/Wait are not exercised by the
// tests in this file and return ErrRuntimeUnsupported/zero values.
type hangingControlSession struct {
	id string
}

var _ backend.SandboxSession = (*hangingControlSession)(nil)

func (s *hangingControlSession) ID() string { return s.id }
func (s *hangingControlSession) Subscribe() ([]byte, <-chan []byte, func(), bool) {
	return nil, nil, func() {}, false
}
func (s *hangingControlSession) WriteInput([]byte) error { return ErrRuntimeUnsupported }
func (s *hangingControlSession) CloseInput() error       { return ErrRuntimeUnsupported }
func (s *hangingControlSession) Resize(backend.TerminalSize) error {
	return ErrRuntimeUnsupported
}
func (s *hangingControlSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, ErrRuntimeUnsupported
}
func (s *hangingControlSession) Stop(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *hangingControlSession) Signal(ctx context.Context, _ syscall.Signal) error {
	<-ctx.Done()
	return ctx.Err()
}

// hangingControlBackend is a backend.SandboxBackend whose Adopt hands back
// a hangingControlSession immediately (this file's tests target the
// Stop/Signal deadline, not the Adopt one — the Adopt engine call's own
// context propagation is already covered by
// TestDetectDaemonStateVolumes_inspectHangsHitsDeadline's sibling coverage
// of containerBackend.doAdopt/ContainerInspect) and whose Launch, when sess
// is set, "starts" that same session successfully — TestRunner_LaunchSandbox_
// PersistFailureStopsSessionUnderDeadline below needs a Launch that
// succeeds so launchSandbox reaches its post-UpdateJob-failure Stop call.
type hangingControlBackend struct {
	sess *hangingControlSession
}

var _ backend.SandboxBackend = (*hangingControlBackend)(nil)

func (b *hangingControlBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	if b.sess == nil {
		return nil, fmt.Errorf("hangingControlBackend.Launch: no session configured")
	}
	return b.sess, nil
}
func (b *hangingControlBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return b.sess, true
}
func (b *hangingControlBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// TestRunner_StopJobRuntime_HangingEngineHitsDeadline pins the runner.go
// StopJobRuntime fix (lines 1487/1491 in the pre-fix numbering): a
// `boid task stop`/task-abort call must not hang forever when the adopted
// session's Stop blocks against a wedged engine.
func TestRunner_StopJobRuntime_HangingEngineHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	start := time.Now()
	r.StopJobRuntime("runtime-xyz")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("StopJobRuntime took %v against an engine that never answers; `boid task stop` would hang", elapsed)
	}
}

// TestRunner_SignalJobRuntime_HangingEngineHitsDeadline is the Signal
// sibling of the Stop test above, pinning the fix at runner.go lines
// 1510/1514 in the pre-fix numbering: `boid agent stop`'s SIGUSR1 delivery
// (NotifyTask → SignalJobRuntime) must not hang forever either.
func TestRunner_SignalJobRuntime_HangingEngineHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	start := time.Now()
	r.SignalJobRuntime("runtime-xyz", syscall.SIGUSR1)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("SignalJobRuntime took %v against an engine that never answers; `boid agent stop` would hang", elapsed)
	}
}

// --- Runner.launchSandbox's post-persist-failure Stop ---------------------

// newClosedTestDB returns a migrated then immediately-closed *sql.DB, so any
// query against it (including UpdateJob's inspectJobColumns) fails
// deterministically with "sql: database is closed" — the mechanism this
// test uses to reach launchSandbox's cleanup-after-UpdateJob-failure branch
// without needing a real persistence fault to occur. Not testutil.NewTestDB:
// testutil transitively imports internal/server (which imports
// internal/dispatcher), so importing it from this internal-package test
// file (package dispatcher, not dispatcher_test) would be an import cycle —
// same reasoning as gitgateway_wire_test.go's newGatewayTestDB, which this
// mirrors.
func newClosedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := d.Conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	return d.Conn
}

// TestRunner_LaunchSandbox_PersistFailureStopsSessionUnderDeadline pins the
// runner.go:1417 fix: when UpdateJob fails right after a successful Launch,
// the best-effort session.Stop cleanup it triggers must not be able to hang
// this dispatch call forever against a wedged engine.
func TestRunner_LaunchSandbox_PersistFailureStopsSessionUnderDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	sess := &hangingControlSession{id: "runtime-persist-fail"}
	r := &Runner{
		DB:      newClosedTestDB(t),
		Backend: &hangingControlBackend{sess: sess},
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "h", Role: string(orchestrator.JobKindHook)}

	start := time.Now()
	_, err := r.launchSandbox(context.Background(), job, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, nil, "", "", "", "", false)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("launchSandbox took %v against an engine that never answers; the dispatch call would hang", elapsed)
	}
	if err == nil {
		t.Fatal("want an error: UpdateJob was forced to fail against a closed DB")
	}
}

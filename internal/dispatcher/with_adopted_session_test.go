//go:build linux

package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file unit-tests withAdoptedSession (runner.go) directly, isolated
// from the 8 call sites it consolidates (Subscribe/WriteInput/
// ResizeRuntime/CloseInput in runtime_subscriber_export.go,
// StopJobRuntime/SignalJobRuntime/CanAttach/ResizeRuntimeID in runner.go —
// docs/plans/refactoring-backlog.md N5A). Those call sites keep their own
// existing coverage in session_control_call_deadline_test.go, which now
// doubles as this refactor's regression guard (it asserts on
// Runner-method-visible behavior — log level, error wrapping, return
// values — none of which was allowed to change). This file instead pins
// the helper's own three-way ctx.Err() classification (legitimate miss /
// Canceled / DeadlineExceeded) and its fn-dispatch contract in isolation,
// using the same fake backends session_control_call_deadline_test.go
// already defines in this package.

// TestWithAdoptedSession_LegitimateMiss_NoCallbackFires pins the routine
// case: Adopt resolves promptly to ok=false with ctx still healthy. Neither
// onDeadlineExceeded nor onCanceled may fire — this is the common "no such
// runtime" result, not something worth logging.
func TestWithAdoptedSession_LegitimateMiss_NoCallbackFires(t *testing.T) {
	r := &Runner{Backend: &notAdoptableBackend{}}

	var deadlineFired, canceledFired bool
	adopted, ctxErr, fnErr := r.withAdoptedSession(context.Background(), "runtime-xyz",
		func(ctx context.Context) { deadlineFired = true },
		func(ctx context.Context) { canceledFired = true },
		func(ctx context.Context, session backend.SandboxSession) error {
			t.Fatal("fn must not run when Adopt fails")
			return nil
		},
	)

	if adopted {
		t.Error("want adopted=false")
	}
	if ctxErr != nil {
		t.Errorf("want ctxErr=nil for a legitimate miss, got %v", ctxErr)
	}
	if fnErr != nil {
		t.Errorf("want fnErr=nil, got %v", fnErr)
	}
	if deadlineFired {
		t.Error("onDeadlineExceeded must not fire for a legitimate miss")
	}
	if canceledFired {
		t.Error("onCanceled must not fire for a legitimate miss")
	}
}

// TestWithAdoptedSession_DeadlineExceeded_OnDeadlineExceededFires pins the
// wedged-engine case: parentCtx is context.Background(), so the only way
// ctx.Err() becomes non-nil is sessionControlCallTimeout firing —
// context.DeadlineExceeded. onDeadlineExceeded must fire, onCanceled must
// not, and ctxErr must wrap context.DeadlineExceeded.
func TestWithAdoptedSession_DeadlineExceeded_OnDeadlineExceededFires(t *testing.T) {
	withSessionControlCallTimeout(t, 100*time.Millisecond)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	var deadlineFired, canceledFired, fnRan bool
	done := make(chan struct{})
	var adopted bool
	var ctxErr, fnErr error
	go func() {
		defer close(done)
		adopted, ctxErr, fnErr = r.withAdoptedSession(context.Background(), "runtime-xyz",
			func(ctx context.Context) { deadlineFired = true },
			func(ctx context.Context) { canceledFired = true },
			func(ctx context.Context, session backend.SandboxSession) error {
				fnRan = true
				return nil
			},
		)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("withAdoptedSession did not return within 5s against a hanging Adopt")
	}

	if fnRan {
		t.Error("fn must not run when Adopt fails")
	}
	if adopted {
		t.Error("want adopted=false")
	}
	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Errorf("want ctxErr to wrap context.DeadlineExceeded, got %v", ctxErr)
	}
	if fnErr != nil {
		t.Errorf("want fnErr=nil, got %v", fnErr)
	}
	if !deadlineFired {
		t.Error("onDeadlineExceeded must fire when the control-call deadline fires")
	}
	if canceledFired {
		t.Error("onCanceled must not fire for a background-derived ctx (only DeadlineExceeded is reachable)")
	}
}

// TestWithAdoptedSession_CallerCanceled_OnCanceledFires pins the
// caller-ctx-derived case: parentCtx is the caller's own ctx (a FLOOR, per
// sessionControlCallTimeout's doc comment), so it can become
// context.Canceled — e.g. an HTTP client disconnecting — distinct from the
// control-call deadline firing. onCanceled must fire, onDeadlineExceeded
// must not, and ctxErr must wrap context.Canceled.
func TestWithAdoptedSession_CallerCanceled_OnCanceledFires(t *testing.T) {
	withSessionControlCallTimeout(t, 5*time.Second) // must not fire before the explicit cancel below
	r := &Runner{Backend: &hangingAdoptBackend{}}

	callerCtx, cancel := context.WithCancel(context.Background())

	var deadlineFired, canceledFired, fnRan bool
	done := make(chan struct{})
	var adopted bool
	var ctxErr, fnErr error
	go func() {
		defer close(done)
		adopted, ctxErr, fnErr = r.withAdoptedSession(callerCtx, "runtime-xyz",
			func(ctx context.Context) { deadlineFired = true },
			func(ctx context.Context) { canceledFired = true },
			func(ctx context.Context, session backend.SandboxSession) error {
				fnRan = true
				return nil
			},
		)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("withAdoptedSession did not return promptly after the caller's ctx was canceled")
	}

	if fnRan {
		t.Error("fn must not run when Adopt fails")
	}
	if adopted {
		t.Error("want adopted=false")
	}
	if !errors.Is(ctxErr, context.Canceled) {
		t.Errorf("want ctxErr to wrap context.Canceled, got %v", ctxErr)
	}
	if fnErr != nil {
		t.Errorf("want fnErr=nil, got %v", fnErr)
	}
	if !canceledFired {
		t.Error("onCanceled must fire when the caller's own ctx is canceled")
	}
	if deadlineFired {
		t.Error("onDeadlineExceeded must not fire for an ordinary caller cancellation")
	}
}

// TestWithAdoptedSession_Adopted_FnRunsUnderBoundedCtx pins the success
// path: Adopt resolving to ok=true must run fn against the resolved
// session and the bounded ctx, and return fn's error verbatim as fnErr —
// neither callback may fire.
func TestWithAdoptedSession_Adopted_FnRunsUnderBoundedCtx(t *testing.T) {
	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	wantErr := errors.New("boom")
	var deadlineFired, canceledFired, fnRan bool
	var gotSession backend.SandboxSession
	var gotCtxHasDeadline bool

	adopted, ctxErr, fnErr := r.withAdoptedSession(context.Background(), "runtime-xyz",
		func(ctx context.Context) { deadlineFired = true },
		func(ctx context.Context) { canceledFired = true },
		func(ctx context.Context, session backend.SandboxSession) error {
			fnRan = true
			gotSession = session
			_, gotCtxHasDeadline = ctx.Deadline()
			return wantErr
		},
	)

	if !adopted {
		t.Fatal("want adopted=true")
	}
	if ctxErr != nil {
		t.Errorf("want ctxErr=nil on success, got %v", ctxErr)
	}
	if !errors.Is(fnErr, wantErr) {
		t.Errorf("want fnErr to be fn's own error, got %v", fnErr)
	}
	if !fnRan {
		t.Fatal("fn must run when Adopt succeeds")
	}
	if gotSession != sess {
		t.Error("fn must receive the session Adopt resolved")
	}
	if !gotCtxHasDeadline {
		t.Error("fn must receive a ctx bounded by sessionControlCallTimeout, not context.Background()")
	}
	if deadlineFired || canceledFired {
		t.Error("neither callback may fire when Adopt succeeds")
	}
}

// TestWithAdoptedSession_Adopted_NilFn pins the CanAttach shape: a nil fn is
// allowed when the caller only cares whether Adopt succeeded (adopted),
// e.g. CanAttach's bool result.
func TestWithAdoptedSession_Adopted_NilFn(t *testing.T) {
	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	adopted, ctxErr, fnErr := r.withAdoptedSession(context.Background(), "runtime-xyz", nil, nil, nil)

	if !adopted {
		t.Fatal("want adopted=true")
	}
	if ctxErr != nil {
		t.Errorf("want ctxErr=nil, got %v", ctxErr)
	}
	if fnErr != nil {
		t.Errorf("want fnErr=nil for a nil fn, got %v", fnErr)
	}
}

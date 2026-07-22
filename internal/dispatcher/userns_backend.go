package dispatcher

import (
	"context"
	"fmt"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// usernsBackend implements backend.SandboxBackend by composing the
// pre-Phase-6 launch path: SandboxPreparer.PrepareSandbox → runnerCommand
// (`boid runner-outer --spec ... --state ...`) → JobRuntime.Start. This is
// an inert extraction (docs/plans/phase6-container-backend.md §PR1):
// behavior is unchanged from the pre-extraction launchSandbox, just
// routed through the SandboxBackend/SandboxSession interface so a second
// (container) backend can be added later (PR5) without touching the
// attach/resize/signal call sites.
//
// usernsBackend owns no state of its own beyond its three dependencies —
// Runner.sandboxBackend() constructs a fresh one on every call rather than
// caching it, since SandboxPreparer/JobRuntime/BoidBinary never change
// after Runner is wired.
type usernsBackend struct {
	preparer   SandboxPreparer
	runtime    JobRuntime
	boidBinary string
}

func newUsernsBackend(preparer SandboxPreparer, runtime JobRuntime, boidBinary string) *usernsBackend {
	return &usernsBackend{preparer: preparer, runtime: runtime, boidBinary: boidBinary}
}

var _ backend.SandboxBackend = (*usernsBackend)(nil)

// usernsStartError distinguishes a Launch failure at the JobRuntime.Start
// phase from a PrepareSandbox-phase failure, entirely for
// Runner.launchSandbox's benefit: the pre-Phase-6 launchSandbox only tore
// down the desiredRuntimeID docker proxy (pre-allocated before Start ran)
// on a Start failure, never on a PrepareSandbox failure. Collapsing both
// phases behind SandboxBackend.Launch's single (session, error) return
// would otherwise lose that distinction; errors.As lets the caller recover
// it without Launch's signature deviating from the interface contract.
// This preserves 振る舞い完全不変 (docs/plans/phase6-container-backend.md
// §PR1) rather than widening when the docker proxy gets torn down.
type usernsStartError struct {
	err error
}

func (e *usernsStartError) Error() string { return fmt.Sprintf("start runtime: %v", e.err) }
func (e *usernsStartError) Unwrap() error { return e.err }

// Launch prepares sandbox artifacts and starts the go-native runner via
// the configured JobRuntime. On any failure it cleans up whatever
// PrepareSandbox produced (scaffolding dirs, spec file, state file) —
// dropping that would leak sandbox artifacts on every Start error.
func (b *usernsBackend) Launch(ctx context.Context, spec sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	prepared, err := b.preparer.PrepareSandbox(spec)
	if err != nil {
		return nil, fmt.Errorf("prepare sandbox: %w", err)
	}
	if prepared == nil || prepared.SpecPath == "" {
		return nil, fmt.Errorf("prepare sandbox: missing spec path")
	}

	handle, err := b.runtime.Start(ctx, RuntimeStartSpec{
		JobID:        opts.JobID,
		TaskID:       opts.TaskID,
		ProjectID:    opts.ProjectID,
		HandlerID:    opts.HandlerID,
		Role:         opts.Role,
		Command:      b.runnerCommand(prepared),
		Interactive:  opts.Interactive,
		TTY:          opts.TTY,
		DesiredID:    opts.DesiredID,
		StdinForward: opts.StdinForward,
	})
	if err != nil {
		cleanupSandboxArtifacts(prepared)
		return nil, &usernsStartError{err: err}
	}

	return &usernsSession{
		runtime:     b.runtime,
		id:          handle.ID,
		interactive: handle.Interactive,
		tty:         handle.TTY,
		prepared:    prepared,
	}, nil
}

// Adopt reconstructs a session for a previously-launched runtimeID. The
// userns backend has no session registry of its own — a JobRuntime +
// runtimeID pair is all SubscribeRuntime/WriteInputRuntime/CloseInputRuntime/
// Resize/Wait/Stop/Signal need, matching the pre-Phase-6 code that resolved
// these directly from r.Runtime.
func (b *usernsBackend) Adopt(_ context.Context, runtimeID string) (backend.SandboxSession, bool) {
	if runtimeID == "" || b.runtime == nil {
		return nil, false
	}
	return &usernsSession{runtime: b.runtime, id: runtimeID}, true
}

// ReapOrphans is a stub in PR1 — the real reap-on-startup implementation
// lands in PR7 (docs/plans/phase6-container-backend.md §PR7).
func (b *usernsBackend) ReapOrphans(_ context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// runnerCommand builds the shell command the runtime executes (via `bash
// -lc`) to launch the go-native sandbox runner:
// `boid runner-outer --spec ... --state ...`. This is the sole place that
// hardcodes the userns entrypoint (docs/plans/phase6-container-backend.md
// 現状棚卸し).
func (b *usernsBackend) runnerCommand(prepared *PreparedSandbox) string {
	boidBin := b.boidBinary
	if boidBin == "" {
		boidBin = "boid"
	}
	return fmt.Sprintf("%s runner-outer --spec %s --state %s",
		shellQuoteDir(boidBin),
		shellQuoteDir(prepared.SpecPath),
		shellQuoteDir(prepared.StatePath),
	)
}

// usernsSession implements backend.SandboxSession over a JobRuntime +
// runtimeID. prepared is non-nil only for sessions returned by Launch —
// Adopt has no PreparedSandbox to hand back (Runner.launchSandbox already
// holds it, via the Launch-returned session, for cleanup purposes).
type usernsSession struct {
	runtime     JobRuntime
	id          string
	interactive bool
	tty         bool
	prepared    *PreparedSandbox
}

var _ backend.SandboxSession = (*usernsSession)(nil)

func (s *usernsSession) ID() string { return s.id }

// Subscribe delegates to LocalRuntime's optional SubscribeRuntime capability
// (not part of the JobRuntime interface — a test JobRuntime stub without it
// makes this gracefully report ok=false, matching the pre-Phase-6 type
// assertion in Runner.Subscribe).
func (s *usernsSession) Subscribe() (snapshot []byte, ch <-chan []byte, cancel func(), ok bool) {
	sub, capable := s.runtime.(interface {
		SubscribeRuntime(string) ([]byte, <-chan []byte, func(), bool)
	})
	if !capable {
		return nil, nil, func() {}, false
	}
	return sub.SubscribeRuntime(s.id)
}

func (s *usernsSession) WriteInput(data []byte) error {
	w, ok := s.runtime.(interface {
		WriteInputRuntime(string, []byte) error
	})
	if !ok {
		return ErrRuntimeUnsupported
	}
	return w.WriteInputRuntime(s.id, data)
}

func (s *usernsSession) CloseInput() error {
	c, ok := s.runtime.(interface {
		CloseInputRuntime(string) error
	})
	if !ok {
		return ErrRuntimeUnsupported
	}
	return c.CloseInputRuntime(s.id)
}

func (s *usernsSession) Resize(size backend.TerminalSize) error {
	return s.runtime.Resize(context.Background(), s.id, size)
}

func (s *usernsSession) Wait(ctx context.Context) (backend.RuntimeExit, error) {
	return s.runtime.Wait(ctx, s.id)
}

func (s *usernsSession) Stop(ctx context.Context) error {
	return s.runtime.Stop(ctx, s.id)
}

func (s *usernsSession) Signal(ctx context.Context, sig syscall.Signal) error {
	return s.runtime.Signal(ctx, s.id, sig)
}

// localArtifacts hands back the on-disk PrepareSandbox output (scaffolding
// dirs, spec/state files) this session's Launch call produced, so Runner's
// post-launch cleanup goroutines can remove them without knowing this is a
// usernsSession — see sessionLocalArtifacts's doc comment for the
// capability-probe contract this participates in. nil for Adopt()-returned
// sessions (see usernsSession's own doc comment: Adopt has no
// PreparedSandbox to hand back).
func (s *usernsSession) localArtifacts() *PreparedSandbox { return s.prepared }

// sessionLocalArtifacts capability-probes session for localArtifacts() —
// the same optional-capability, type-assert-with-ok pattern usernsSession
// itself already uses to probe JobRuntime for SubscribeRuntime/
// WriteInputRuntime/CloseInputRuntime (see Subscribe/WriteInput/CloseInput
// above). Runner's launchSandbox / cleanupSandboxAfterWait use this instead
// of asserting session to a concrete *usernsSession, so a future container
// backend's session (PR5, docs/plans/phase6-container-backend.md) — which
// has no local scaffolding/spec/state files to clean up — simply doesn't
// implement it and this returns nil rather than a hard failure.
func sessionLocalArtifacts(session backend.SandboxSession) *PreparedSandbox {
	if la, ok := session.(interface{ localArtifacts() *PreparedSandbox }); ok {
		return la.localArtifacts()
	}
	return nil
}

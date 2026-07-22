package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins the SandboxBackend/SandboxSession contract
// (docs/plans/phase6-container-backend.md §PR1) as implemented by
// usernsBackend/usernsSession — the extraction of the pre-Phase-6 launch
// path (SandboxPreparer.PrepareSandbox → runnerCommand → JobRuntime.Start)
// behind the new interface.

// ubFakePreparer is a minimal SandboxPreparer stub for usernsBackend tests.
type ubFakePreparer struct {
	prepared *PreparedSandbox
	prepErr  error
}

func newUBFakePreparer(t *testing.T) *ubFakePreparer {
	t.Helper()
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "root")
	stagingDir := filepath.Join(dir, "staging")
	specPath := filepath.Join(dir, "spec.json")
	statePath := filepath.Join(dir, "state.json")
	for _, d := range []string{rootDir, stagingDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(specPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return &ubFakePreparer{
		prepared: &PreparedSandbox{
			SpecPath:   specPath,
			StatePath:  statePath,
			RootDir:    rootDir,
			StagingDir: stagingDir,
		},
	}
}

func (p *ubFakePreparer) PrepareSandbox(_ sandbox.Spec) (*PreparedSandbox, error) {
	if p.prepErr != nil {
		return nil, p.prepErr
	}
	return p.prepared, nil
}

type ubResizeCall struct {
	id   string
	size TerminalSize
}

type ubSignalCall struct {
	id  string
	sig syscall.Signal
}

// ubFakeRuntime implements the core JobRuntime interface only — no
// SubscribeRuntime/WriteInputRuntime/CloseInputRuntime — so tests can
// exercise usernsSession's "not capable" branches.
type ubFakeRuntime struct {
	startSpec RuntimeStartSpec
	startErr  error
	handleID  string

	stopCalls []string
	stopErr   error

	signalCalls []ubSignalCall

	resizeCalls []ubResizeCall
	resizeErr   error

	waitExit RuntimeExit
	waitErr  error
}

func (r *ubFakeRuntime) Start(_ context.Context, spec RuntimeStartSpec) (*RuntimeHandle, error) {
	r.startSpec = spec
	if r.startErr != nil {
		return nil, r.startErr
	}
	id := r.handleID
	if id == "" {
		id = "ub-runtime-1"
	}
	return &RuntimeHandle{ID: id, Interactive: spec.Interactive, TTY: spec.TTY}, nil
}

func (r *ubFakeRuntime) Attach(_ context.Context, _ string, _ RuntimeAttachRequest) error {
	return ErrRuntimeUnsupported
}

func (r *ubFakeRuntime) Resize(_ context.Context, runtimeID string, size TerminalSize) error {
	r.resizeCalls = append(r.resizeCalls, ubResizeCall{id: runtimeID, size: size})
	return r.resizeErr
}

func (r *ubFakeRuntime) Wait(_ context.Context, _ string) (RuntimeExit, error) {
	return r.waitExit, r.waitErr
}

func (r *ubFakeRuntime) Stop(_ context.Context, runtimeID string) error {
	r.stopCalls = append(r.stopCalls, runtimeID)
	return r.stopErr
}

func (r *ubFakeRuntime) Signal(_ context.Context, runtimeID string, sig syscall.Signal) error {
	r.signalCalls = append(r.signalCalls, ubSignalCall{id: runtimeID, sig: sig})
	return nil
}

// ubFakeCapableRuntime additionally implements the optional
// SubscribeRuntime/WriteInputRuntime/CloseInputRuntime capability methods
// usernsSession probes for via type assertion (mirroring LocalRuntime),
// so tests can exercise the "capable" delegation branches.
type ubFakeCapableRuntime struct {
	ubFakeRuntime

	subCalls []string
	subSnap  []byte
	subCh    chan []byte
	subOK    bool

	writeInputCalls []struct {
		id   string
		data []byte
	}
	writeInputErr error

	closeInputCalls []string
	closeInputErr   error
}

func (r *ubFakeCapableRuntime) SubscribeRuntime(runtimeID string) ([]byte, <-chan []byte, func(), bool) {
	r.subCalls = append(r.subCalls, runtimeID)
	return r.subSnap, r.subCh, func() {}, r.subOK
}

func (r *ubFakeCapableRuntime) WriteInputRuntime(runtimeID string, data []byte) error {
	r.writeInputCalls = append(r.writeInputCalls, struct {
		id   string
		data []byte
	}{runtimeID, append([]byte(nil), data...)})
	return r.writeInputErr
}

func (r *ubFakeCapableRuntime) CloseInputRuntime(runtimeID string) error {
	r.closeInputCalls = append(r.closeInputCalls, runtimeID)
	return r.closeInputErr
}

func TestUsernsBackend_Launch_Success(t *testing.T) {
	prep := newUBFakePreparer(t)
	rt := &ubFakeRuntime{handleID: "runtime-xyz"}
	b := newUsernsBackend(prep, rt, "/opt/boid")

	session, err := b.Launch(context.Background(), sandbox.Spec{}, backend.LaunchOptions{
		JobID:        "job-1",
		TaskID:       "task-1",
		ProjectID:    "proj-1",
		HandlerID:    "hook-a",
		Role:         "hook",
		Interactive:  true,
		TTY:          true,
		DesiredID:    "desired-id",
		StdinForward: true,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if session == nil {
		t.Fatal("Launch returned nil session with nil error")
	}
	if got, want := session.ID(), "runtime-xyz"; got != want {
		t.Errorf("session.ID() = %q, want %q", got, want)
	}

	// The command hardcodes the userns entrypoint — this is the one place
	// that does (docs/plans/phase6-container-backend.md 現状棚卸し).
	wantCmd := "/opt/boid runner-outer --spec " + shellQuoteDir(prep.prepared.SpecPath) +
		" --state " + shellQuoteDir(prep.prepared.StatePath)
	if rt.startSpec.Command != wantCmd {
		t.Errorf("Start Command = %q, want %q", rt.startSpec.Command, wantCmd)
	}
	if rt.startSpec.JobID != "job-1" || rt.startSpec.TaskID != "task-1" ||
		rt.startSpec.ProjectID != "proj-1" || rt.startSpec.HandlerID != "hook-a" || rt.startSpec.Role != "hook" {
		t.Errorf("Start identity fields not propagated from LaunchOptions: %+v", rt.startSpec)
	}
	if !rt.startSpec.Interactive || !rt.startSpec.TTY || !rt.startSpec.StdinForward {
		t.Errorf("Start Interactive/TTY/StdinForward not propagated: %+v", rt.startSpec)
	}
	if rt.startSpec.DesiredID != "desired-id" {
		t.Errorf("Start DesiredID = %q, want %q", rt.startSpec.DesiredID, "desired-id")
	}
}

func TestUsernsBackend_Launch_DefaultsBoidBinary(t *testing.T) {
	prep := newUBFakePreparer(t)
	rt := &ubFakeRuntime{}
	b := newUsernsBackend(prep, rt, "") // empty BoidBinary falls back to "boid"

	if _, err := b.Launch(context.Background(), sandbox.Spec{}, backend.LaunchOptions{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	wantPrefix := "boid runner-outer --spec "
	if len(rt.startSpec.Command) < len(wantPrefix) || rt.startSpec.Command[:len(wantPrefix)] != wantPrefix {
		t.Errorf("Start Command = %q, want prefix %q", rt.startSpec.Command, wantPrefix)
	}
}

func TestUsernsBackend_Launch_PrepareError_NoCleanupNoStartPhaseMarker(t *testing.T) {
	prep := newUBFakePreparer(t)
	prep.prepErr = errors.New("boom: prepare failed")
	rt := &ubFakeRuntime{}
	b := newUsernsBackend(prep, rt, "boid")

	session, err := b.Launch(context.Background(), sandbox.Spec{}, backend.LaunchOptions{})
	if err == nil {
		t.Fatal("expected Launch to fail when PrepareSandbox fails")
	}
	if session != nil {
		t.Errorf("expected nil session on PrepareSandbox failure, got %v", session)
	}

	// A PrepareSandbox-phase failure must NOT be reported as a
	// usernsStartError — Runner.launchSandbox relies on that distinction to
	// preserve the pre-Phase-6 behavior of only tearing down the
	// desiredRuntimeID docker proxy on a Start-phase failure.
	var startErr *usernsStartError
	if errors.As(err, &startErr) {
		t.Error("PrepareSandbox failure must not be classified as a usernsStartError")
	}
	// Start must never have been reached.
	if rt.startSpec.Command != "" {
		t.Errorf("JobRuntime.Start should not have been called, got Command %q", rt.startSpec.Command)
	}
}

func TestUsernsBackend_Launch_StartError_CleansUpArtifactsAndMarksStartPhase(t *testing.T) {
	prep := newUBFakePreparer(t)
	startErrCause := errors.New("boom: start failed")
	rt := &ubFakeRuntime{startErr: startErrCause}
	b := newUsernsBackend(prep, rt, "boid")

	session, err := b.Launch(context.Background(), sandbox.Spec{}, backend.LaunchOptions{})
	if err == nil {
		t.Fatal("expected Launch to fail when JobRuntime.Start fails")
	}
	if session != nil {
		t.Errorf("expected nil session on Start failure, got %v", session)
	}

	var startErr *usernsStartError
	if !errors.As(err, &startErr) {
		t.Fatalf("expected a *usernsStartError, got %T: %v", err, err)
	}
	if !errors.Is(err, startErrCause) {
		t.Errorf("wrapped error chain should include the original Start error; err = %v", err)
	}

	// Cleanup responsibility must not be dropped on extraction (plan §PR1).
	for _, p := range []string{prep.prepared.RootDir, prep.prepared.StagingDir, prep.prepared.SpecPath, prep.prepared.StatePath} {
		if _, statErr := os.Stat(p); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("expected %s removed after Start failure, stat err = %v", p, statErr)
		}
	}
}

func TestUsernsBackend_Launch_MissingSpecPath(t *testing.T) {
	prep := &ubFakePreparer{prepared: &PreparedSandbox{}} // SpecPath == ""
	rt := &ubFakeRuntime{}
	b := newUsernsBackend(prep, rt, "boid")

	if _, err := b.Launch(context.Background(), sandbox.Spec{}, backend.LaunchOptions{}); err == nil {
		t.Fatal("expected Launch to fail when PrepareSandbox returns an empty SpecPath")
	}
}

func TestUsernsBackend_Adopt(t *testing.T) {
	rt := &ubFakeRuntime{}
	b := newUsernsBackend(nil, rt, "boid")

	if _, ok := b.Adopt(context.Background(), ""); ok {
		t.Error("Adopt(\"\") should report ok=false")
	}

	session, ok := b.Adopt(context.Background(), "runtime-42")
	if !ok {
		t.Fatal("Adopt with a non-empty runtimeID and configured runtime should succeed")
	}
	if got := session.ID(); got != "runtime-42" {
		t.Errorf("session.ID() = %q, want %q", got, "runtime-42")
	}

	nilRuntimeBackend := newUsernsBackend(nil, nil, "boid")
	if _, ok := nilRuntimeBackend.Adopt(context.Background(), "runtime-42"); ok {
		t.Error("Adopt with a nil JobRuntime should report ok=false")
	}
}

func TestUsernsBackend_ReapOrphans_StubReturnsZeroReport(t *testing.T) {
	b := newUsernsBackend(nil, &ubFakeRuntime{}, "boid")
	report, err := b.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(report.ReapedJobIDs) != 0 || len(report.FailedJobIDs) != 0 || report.GlobalError != nil {
		t.Errorf("ReapOrphans should stub to a zero ReapReport in PR1 (real reap lands in PR7), got %+v", report)
	}
}

func TestUsernsSession_Subscribe_NotCapable(t *testing.T) {
	rt := &ubFakeRuntime{} // no SubscribeRuntime method
	s := &usernsSession{runtime: rt, id: "rt-1"}

	snap, ch, cancel, ok := s.Subscribe()
	if ok || snap != nil || ch != nil {
		t.Errorf("Subscribe on an incapable runtime should report ok=false with nil snapshot/channel, got snap=%v ch=%v ok=%v", snap, ch, ok)
	}
	cancel() // must not panic even though there is nothing to cancel
}

func TestUsernsSession_Subscribe_DelegatesToCapableRuntime(t *testing.T) {
	rt := &ubFakeCapableRuntime{subSnap: []byte("hello"), subCh: make(chan []byte, 1), subOK: true}
	s := &usernsSession{runtime: rt, id: "rt-2"}

	snap, ch, cancel, ok := s.Subscribe()
	defer cancel()
	if !ok {
		t.Fatal("Subscribe should delegate to the capable runtime and report ok=true")
	}
	if string(snap) != "hello" {
		t.Errorf("snapshot = %q, want %q", snap, "hello")
	}
	if ch == nil {
		t.Error("channel should be the one returned by SubscribeRuntime")
	}
	if len(rt.subCalls) != 1 || rt.subCalls[0] != "rt-2" {
		t.Errorf("SubscribeRuntime should have been called with the session's id, got %v", rt.subCalls)
	}
}

func TestUsernsSession_WriteInput(t *testing.T) {
	incapable := &usernsSession{runtime: &ubFakeRuntime{}, id: "rt-1"}
	if err := incapable.WriteInput([]byte("x")); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("WriteInput on an incapable runtime = %v, want ErrRuntimeUnsupported", err)
	}

	rt := &ubFakeCapableRuntime{}
	capable := &usernsSession{runtime: rt, id: "rt-2"}
	if err := capable.WriteInput([]byte("ls\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	if len(rt.writeInputCalls) != 1 || rt.writeInputCalls[0].id != "rt-2" || string(rt.writeInputCalls[0].data) != "ls\n" {
		t.Errorf("WriteInputRuntime not called as expected: %+v", rt.writeInputCalls)
	}
}

func TestUsernsSession_CloseInput(t *testing.T) {
	incapable := &usernsSession{runtime: &ubFakeRuntime{}, id: "rt-1"}
	if err := incapable.CloseInput(); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("CloseInput on an incapable runtime = %v, want ErrRuntimeUnsupported", err)
	}

	rt := &ubFakeCapableRuntime{}
	capable := &usernsSession{runtime: rt, id: "rt-2"}
	if err := capable.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}
	if len(rt.closeInputCalls) != 1 || rt.closeInputCalls[0] != "rt-2" {
		t.Errorf("CloseInputRuntime not called as expected: %v", rt.closeInputCalls)
	}
}

func TestUsernsSession_Resize_AlwaysDelegatesToJobRuntimeResize(t *testing.T) {
	// Resize is part of the core JobRuntime interface (unlike Subscribe/
	// WriteInput/CloseInput), so it must always be called through — no
	// capability probing.
	rt := &ubFakeRuntime{}
	s := &usernsSession{runtime: rt, id: "rt-3"}

	if err := s.Resize(TerminalSize{Rows: 40, Cols: 120}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if len(rt.resizeCalls) != 1 || rt.resizeCalls[0].id != "rt-3" || rt.resizeCalls[0].size != (TerminalSize{Rows: 40, Cols: 120}) {
		t.Errorf("Resize not delegated as expected: %+v", rt.resizeCalls)
	}
}

func TestUsernsSession_Wait(t *testing.T) {
	rt := &ubFakeRuntime{waitExit: RuntimeExit{ExitCode: 7, TranscriptPath: "/tmp/t.log"}}
	s := &usernsSession{runtime: rt, id: "rt-4"}

	exit, err := s.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.ExitCode != 7 || exit.TranscriptPath != "/tmp/t.log" {
		t.Errorf("Wait result = %+v, want ExitCode=7 TranscriptPath=/tmp/t.log", exit)
	}
}

func TestUsernsSession_Stop(t *testing.T) {
	rt := &ubFakeRuntime{}
	s := &usernsSession{runtime: rt, id: "rt-5"}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(rt.stopCalls) != 1 || rt.stopCalls[0] != "rt-5" {
		t.Errorf("Stop not delegated as expected: %v", rt.stopCalls)
	}
}

func TestUsernsSession_Signal(t *testing.T) {
	rt := &ubFakeRuntime{}
	s := &usernsSession{runtime: rt, id: "rt-6"}

	if err := s.Signal(context.Background(), syscall.SIGUSR1); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if len(rt.signalCalls) != 1 || rt.signalCalls[0].id != "rt-6" || rt.signalCalls[0].sig != syscall.SIGUSR1 {
		t.Errorf("Signal not delegated as expected: %+v", rt.signalCalls)
	}
}

// Compile-time interface pins, mirroring the ones in userns_backend.go —
// kept here too so a future edit that accidentally breaks one of these
// (e.g. changing usernsBackend's method set) fails at `go build`, not just
// at the point something happens to instantiate backend.SandboxBackend.
var (
	_ backend.SandboxBackend = (*usernsBackend)(nil)
	_ backend.SandboxSession = (*usernsSession)(nil)
)

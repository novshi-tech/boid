package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// waitableRuntime is a JobRuntime stub whose Wait returns a configurable
// RuntimeExit. Used to drive cleanupSandboxAfterWait through the success and
// failure branches.
type waitableRuntime struct {
	exit RuntimeExit
	err  error
}

func (r *waitableRuntime) Start(_ context.Context, _ RuntimeStartSpec) (*RuntimeHandle, error) {
	return nil, ErrRuntimeUnsupported
}
func (r *waitableRuntime) Attach(_ context.Context, _ string, _ RuntimeAttachRequest) error {
	return ErrRuntimeUnsupported
}
func (r *waitableRuntime) Resize(_ context.Context, _ string, _ TerminalSize) error {
	return ErrRuntimeUnsupported
}
func (r *waitableRuntime) Wait(_ context.Context, _ string) (RuntimeExit, error) {
	return r.exit, r.err
}
func (r *waitableRuntime) Stop(_ context.Context, _ string) error {
	return nil
}
func (r *waitableRuntime) Signal(_ context.Context, _ string, _ syscall.Signal) error {
	return nil
}

func makePreparedFixture(t *testing.T) *PreparedSandbox {
	t.Helper()
	dir := t.TempDir()

	rootDir := filepath.Join(dir, "boid-root-XXX")
	stagingDir := filepath.Join(dir, "boid-gates-YYY")
	outerPath := filepath.Join(dir, "boid-job-outer.sh")
	setupPath := filepath.Join(dir, "boid-job-setup.sh")
	innerPath := filepath.Join(dir, "boid-job-inner.sh")

	for _, d := range []string{rootDir, stagingDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, f := range []string{outerPath, setupPath, innerPath} {
		if err := os.WriteFile(f, []byte("#!/bin/bash\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return &PreparedSandbox{
		OuterPath:   outerPath,
		RootDir:     rootDir,
		ScriptPaths: []string{outerPath, setupPath, innerPath},
		StagingDir:  stagingDir,
	}
}

func TestCleanupSandboxAfterWait_RemovesArtifactsOnSuccess(t *testing.T) {
	prep := makePreparedFixture(t)
	r := &Runner{Runtime: &waitableRuntime{exit: RuntimeExit{ExitCode: 0}}}

	r.cleanupSandboxAfterWait("rt-success", prep, nil)

	for _, p := range append([]string{prep.RootDir, prep.StagingDir}, prep.ScriptPaths...) {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s removed on exit_code=0, stat err = %v", p, err)
		}
	}
}

// silent exit_code=1 の事後解析を可能にするため、 失敗時は **script ファイルだけ**
// 残す。 rootDir / stagingDir は中身が無いので保全しても診断材料にならず、 旧来は
// setup.sh の cleanup trap で消えず leak していたため意図的に削除に変更。
func TestCleanupSandboxAfterWait_RetainsScriptsOnFailure(t *testing.T) {
	prep := makePreparedFixture(t)
	r := &Runner{Runtime: &waitableRuntime{exit: RuntimeExit{ExitCode: 1}}}

	r.cleanupSandboxAfterWait("rt-failed", prep, nil)

	// Scaffolding must be removed (outer.sh は失敗時もこれを rm するが、
	// daemon は保険として idempotent に同じことをする)。
	for _, p := range []string{prep.RootDir, prep.StagingDir} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s removed on exit_code!=0, stat err = %v", p, err)
		}
	}
	// Script ファイルは事後解析のため保全する。
	for _, p := range prep.ScriptPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected script %s retained on exit_code!=0, stat err = %v", p, err)
		}
	}
}

func TestTranscriptSizeBytes(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	withData := filepath.Join(dir, "data.log")
	if err := os.WriteFile(withData, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}

	if size, msg := transcriptSizeBytes(""); size != -1 || msg == "" {
		t.Errorf("empty path: got (%d,%q), want (-1, non-empty)", size, msg)
	}
	if size, msg := transcriptSizeBytes(filepath.Join(dir, "missing.log")); size != -1 || msg == "" {
		t.Errorf("missing path: got (%d,%q), want (-1, non-empty)", size, msg)
	}
	if size, msg := transcriptSizeBytes(empty); size != 0 || msg != "" {
		t.Errorf("empty file: got (%d,%q), want (0,'')", size, msg)
	}
	if size, msg := transcriptSizeBytes(withData); size != 5 || msg != "" {
		t.Errorf("5-byte file: got (%d,%q), want (5,'')", size, msg)
	}
}

func TestCleanupSandboxAfterWait_RunsExtraCleanupAlways(t *testing.T) {
	cases := []struct {
		name string
		exit RuntimeExit
	}{
		{"success", RuntimeExit{ExitCode: 0}},
		{"failure", RuntimeExit{ExitCode: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prep := makePreparedFixture(t)
			called := false
			r := &Runner{Runtime: &waitableRuntime{exit: tc.exit}}

			r.cleanupSandboxAfterWait("rt-x", prep, func() { called = true })

			if !called {
				t.Errorf("extra cleanup must run regardless of exit code (case=%s)", tc.name)
			}
		})
	}
}

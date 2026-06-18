package dispatcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestCleanupSandboxArtifacts_RemovesRootSpecStateAndStaging(t *testing.T) {
	dir := t.TempDir()

	rootDir := filepath.Join(dir, "boid-root-XXX")
	stagingDir := filepath.Join(dir, "boid-staging-YYY")
	specPath := filepath.Join(dir, "boid-job-runner-spec.json")
	statePath := filepath.Join(dir, "boid-job-runner-state.json")

	for _, d := range []string{rootDir, stagingDir} {
		if err := os.MkdirAll(filepath.Join(d, "nested"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, f := range []string{specPath, statePath} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	cleanupSandboxArtifacts(&PreparedSandbox{
		SpecPath:   specPath,
		StatePath:  statePath,
		RootDir:    rootDir,
		StagingDir: stagingDir,
	})

	for _, p := range []string{rootDir, stagingDir, specPath, statePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat error = %v", p, err)
		}
	}
}

func TestCleanupSandboxArtifacts_NilSafe(t *testing.T) {
	cleanupSandboxArtifacts(nil)
	cleanupSandboxArtifacts(&PreparedSandbox{})
}

func TestCleanupSandboxArtifacts_MissingFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "boid-job-runner-spec.json")
	if err := os.WriteFile(existing, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	missing := filepath.Join(dir, "boid-missing-runner-state.json")

	cleanupSandboxArtifacts(&PreparedSandbox{
		SpecPath:  existing,
		StatePath: missing,
	})

	if _, err := os.Stat(existing); !os.IsNotExist(err) {
		t.Errorf("existing spec should be removed, got err = %v", err)
	}
}

func TestSandboxPreparer_PopulatesCleanupFields(t *testing.T) {
	stagingDir := "/tmp/boid-staging-preparertest"
	spec := sandbox.Spec{
		ID:      "prep-test-job",
		WorkDir: "/host/project",
		Env:     map[string]string{"HOME": "/host/home"},
		Argv:    []string{"/bin/true"},
		Mounts: []sandbox.Mount{
			{Source: "/usr/local/bin/boid", Target: "/opt/boid/bin/boid", Type: sandbox.MountBind, IsFile: true, ReadOnly: true},
		},
		CleanupPaths: []string{stagingDir},
	}

	prep, err := NewSandboxPreparer().PrepareSandbox(spec)
	if err != nil {
		t.Fatalf("PrepareSandbox: %v", err)
	}
	t.Cleanup(func() { cleanupSandboxArtifacts(prep) })

	if prep.RootDir == "" {
		t.Error("PreparedSandbox.RootDir should be set")
	}
	if info, err := os.Stat(prep.RootDir); err != nil || !info.IsDir() {
		t.Errorf("RootDir %q should exist as directory, stat err = %v", prep.RootDir, err)
	}
	if prep.SpecPath == "" {
		t.Error("PreparedSandbox.SpecPath should be set")
	}
	if prep.StatePath == "" {
		t.Error("PreparedSandbox.StatePath should be set")
	}
	if prep.StagingDir != stagingDir {
		t.Errorf("StagingDir = %q, want %q", prep.StagingDir, stagingDir)
	}
	// The spec file is materialized; the state file is created lazily by the runner.
	if _, err := os.Stat(prep.SpecPath); err != nil {
		t.Errorf("spec file %q should exist, err = %v", prep.SpecPath, err)
	}
}

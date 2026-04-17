package dispatcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupSandboxArtifacts_RemovesRootScriptsAndStaging(t *testing.T) {
	dir := t.TempDir()

	rootDir := filepath.Join(dir, "boid-root-XXX")
	stagingDir := filepath.Join(dir, "boid-gates-YYY")
	outerPath := filepath.Join(dir, "boid-job-outer.sh")
	setupPath := filepath.Join(dir, "boid-job-setup.sh")
	innerPath := filepath.Join(dir, "boid-job-inner.sh")

	for _, d := range []string{rootDir, stagingDir} {
		if err := os.MkdirAll(filepath.Join(d, "nested"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, f := range []string{outerPath, setupPath, innerPath} {
		if err := os.WriteFile(f, []byte("#!/bin/bash\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	cleanupSandboxArtifacts(&PreparedSandbox{
		OuterPath:   outerPath,
		RootDir:     rootDir,
		ScriptPaths: []string{outerPath, setupPath, innerPath},
		StagingDir:  stagingDir,
	})

	for _, p := range []string{rootDir, stagingDir, outerPath, setupPath, innerPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat error = %v", p, err)
		}
	}
}

func TestCleanupSandboxArtifacts_NilSafe(t *testing.T) {
	cleanupSandboxArtifacts(nil)
	cleanupSandboxArtifacts(&PreparedSandbox{})
}

func TestCleanupSandboxArtifacts_MissingScriptIsIgnored(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.sh")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	missing := filepath.Join(dir, "missing.sh")

	cleanupSandboxArtifacts(&PreparedSandbox{
		ScriptPaths: []string{existing, missing},
	})

	if _, err := os.Stat(existing); !os.IsNotExist(err) {
		t.Errorf("existing script should be removed, got err = %v", err)
	}
}

func TestSandboxPreparer_PopulatesCleanupFields(t *testing.T) {
	spec := SandboxSpec{
		JobID:        "prep-test-job",
		ProjectID:    "proj",
		ProjectDir:   "/host/project",
		HomeDir:      "/host/home",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid.sock",
		Role:         "gate",
		StagingDir:   "/tmp/boid-gates-preparertest",
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
	if prep.OuterPath == "" {
		t.Error("PreparedSandbox.OuterPath should be set")
	}
	if len(prep.ScriptPaths) != 3 {
		t.Errorf("expected 3 ScriptPaths, got %d: %v", len(prep.ScriptPaths), prep.ScriptPaths)
	}
	if prep.StagingDir != spec.StagingDir {
		t.Errorf("StagingDir = %q, want %q", prep.StagingDir, spec.StagingDir)
	}
	for _, p := range prep.ScriptPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("script %q should exist, err = %v", p, err)
		}
	}
}

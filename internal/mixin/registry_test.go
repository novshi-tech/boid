package mixin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/mixin"
)

func TestRegistry_Resolve(t *testing.T) {
	baseDir := t.TempDir()

	// Create a fake mixin at baseDir/github.com/user/repo/go/mixin.yaml
	mixinDir := filepath.Join(baseDir, "github.com", "user", "repo", "go")
	os.MkdirAll(mixinDir, 0o755)
	os.WriteFile(filepath.Join(mixinDir, "mixin.yaml"), []byte("env: {}"), 0o644)

	reg := mixin.NewRegistry(baseDir)

	path, err := reg.Resolve("github.com/user/repo/go")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if path != mixinDir {
		t.Errorf("path = %q, want %q", path, mixinDir)
	}
}

func TestRegistry_Resolve_NotFound(t *testing.T) {
	reg := mixin.NewRegistry(t.TempDir())
	_, err := reg.Resolve("github.com/user/repo/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent mixin")
	}
}

func TestRegistry_Resolve_ShortRef(t *testing.T) {
	reg := mixin.NewRegistry(t.TempDir())
	_, err := reg.Resolve("too/short")
	if err == nil {
		t.Fatal("expected error for short ref")
	}
}

func TestRegistry_Install(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create work repo with a mixin, then make it bare
	workDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(workDir, 0o755)

	git := func(args ...string) {
		t.Helper()
		allArgs := append([]string{"-C", workDir}, args...)
		cmd := exec.Command("git", allArgs...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed (sandbox?): %s", args, out)
		}
	}

	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")

	mixinDir := filepath.Join(workDir, "go")
	os.MkdirAll(mixinDir, 0o755)
	os.WriteFile(filepath.Join(mixinDir, "mixin.yaml"), []byte("env:\n  GOPATH: /go"), 0o644)
	git("add", ".")
	git("commit", "-m", "init")

	// Clone to bare repo
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "clone", "--bare", workDir, remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git clone --bare failed: %s", out)
	}
	_ = cmd

	// Install
	baseDir := t.TempDir()
	reg := mixin.NewRegistry(baseDir)

	err := reg.InstallFromURL("test-host/user/repo", remoteDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Resolve should work now
	path, err := reg.Resolve("test-host/user/repo/go")
	if err != nil {
		t.Fatalf("Resolve after install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "mixin.yaml")); err != nil {
		t.Errorf("mixin.yaml not found at %s", path)
	}
}

func TestRegistry_List(t *testing.T) {
	baseDir := t.TempDir()

	// Create fake repo dirs
	os.MkdirAll(filepath.Join(baseDir, "github.com", "user", "repo1", ".git"), 0o755)
	os.MkdirAll(filepath.Join(baseDir, "github.com", "user", "repo2", ".git"), 0o755)

	reg := mixin.NewRegistry(baseDir)
	repos, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(repos) != 2 {
		t.Errorf("repos = %v, want 2 entries", repos)
	}
}

func TestRegistry_Remove(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "github.com", "user", "repo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755)

	reg := mixin.NewRegistry(baseDir)
	if err := reg.Remove("github.com/user/repo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Error("repo dir should be removed")
	}
}

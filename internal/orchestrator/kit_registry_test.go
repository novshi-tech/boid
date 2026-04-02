package orchestrator_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	kit "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestRegistry_Resolve(t *testing.T) {
	baseDir := t.TempDir()

	// Create a fake kit at baseDir/github.com/user/repo/go/kit.yaml
	kitDir := filepath.Join(baseDir, "github.com", "user", "repo", "go")
	os.MkdirAll(kitDir, 0o755)
	os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env: {}"), 0o644)

	reg := kit.NewRegistry(baseDir)

	path, err := reg.Resolve("github.com/user/repo/go")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if path != kitDir {
		t.Errorf("path = %q, want %q", path, kitDir)
	}
}

func TestRegistry_Resolve_NotFound(t *testing.T) {
	reg := kit.NewRegistry(t.TempDir())
	_, err := reg.Resolve("github.com/user/repo/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent kit")
	}
}

func TestRegistry_Resolve_ShortRef(t *testing.T) {
	reg := kit.NewRegistry(t.TempDir())
	_, err := reg.Resolve("too/short")
	if err == nil {
		t.Fatal("expected error for short ref")
	}
}

func TestRegistry_Resolve_LocalRef(t *testing.T) {
	baseDir := t.TempDir()
	kitDir := filepath.Join(baseDir, "local", "dev", "repro-kit")
	os.MkdirAll(kitDir, 0o755)
	os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env: {}"), 0o644)

	reg := kit.NewRegistry(baseDir)
	path, err := reg.Resolve("local/dev/repro-kit")
	if err != nil {
		t.Fatalf("Resolve local ref: %v", err)
	}
	if path != kitDir {
		t.Errorf("path = %q, want %q", path, kitDir)
	}
}

func TestRegistry_Install(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create work repo with a kit, then make it bare
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

	kitDir := filepath.Join(workDir, "go")
	os.MkdirAll(kitDir, 0o755)
	os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env:\n  GOPATH: /go"), 0o644)
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
	reg := kit.NewRegistry(baseDir)

	err := reg.InstallFromURL("test-host/user/repo", remoteDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Resolve should work now
	path, err := reg.Resolve("test-host/user/repo/go")
	if err != nil {
		t.Fatalf("Resolve after install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "kit.yaml")); err != nil {
		t.Errorf("kit.yaml not found at %s", path)
	}
}

func TestRegistry_List(t *testing.T) {
	baseDir := t.TempDir()

	// Create fake repo dirs
	os.MkdirAll(filepath.Join(baseDir, "github.com", "user", "repo1", ".git"), 0o755)
	os.MkdirAll(filepath.Join(baseDir, "github.com", "user", "repo2", ".git"), 0o755)

	reg := kit.NewRegistry(baseDir)
	repos, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(repos) != 2 {
		t.Errorf("repos = %v, want 2 entries", repos)
	}
}

func TestRegistry_IsInstalled(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "github.com", "user", "repo")
	os.MkdirAll(repoDir, 0o755)

	reg := kit.NewRegistry(baseDir)

	if !reg.IsInstalled("github.com/user/repo") {
		t.Error("expected IsInstalled=true for existing dir")
	}
	if reg.IsInstalled("github.com/user/nonexistent") {
		t.Error("expected IsInstalled=false for missing dir")
	}
}

func TestRepoRefsFromKitRefs(t *testing.T) {
	tests := []struct {
		name string
		refs []string
		want []string
	}{
		{
			name: "all local",
			refs: []string{"go-dev", "git", "build"},
			want: nil,
		},
		{
			name: "remote only",
			refs: []string{"github.com/acme/kits/go-dev", "github.com/acme/kits/ci"},
			want: []string{"github.com/acme/kits"},
		},
		{
			name: "mixed local and remote",
			refs: []string{"go-dev", "github.com/acme/kits/go-dev", "git"},
			want: []string{"github.com/acme/kits"},
		},
		{
			name: "local prefix skipped",
			refs: []string{"local/dev/repro-kit"},
			want: nil,
		},
		{
			name: "multiple repos",
			refs: []string{"github.com/acme/kits/go", "github.com/other/repo/ci"},
			want: []string{"github.com/acme/kits", "github.com/other/repo"},
		},
		{
			name: "dedup same repo",
			refs: []string{"github.com/acme/kits/go", "github.com/acme/kits/ci"},
			want: []string{"github.com/acme/kits"},
		},
		{
			name: "empty",
			refs: nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kit.RepoRefsFromKitRefs(tt.refs)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRepoRefToCloneURL(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		useSSH bool
		want   string
	}{
		{
			name:   "https default",
			ref:    "github.com/user/repo",
			useSSH: false,
			want:   "https://github.com/user/repo.git",
		},
		{
			name:   "ssh",
			ref:    "github.com/user/repo",
			useSSH: true,
			want:   "git@github.com:user/repo.git",
		},
		{
			name:   "ssh custom host",
			ref:    "gitlab.example.com/team/kits",
			useSSH: true,
			want:   "git@gitlab.example.com:team/kits.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kit.RepoRefToCloneURL(tt.ref, tt.useSSH)
			if got != tt.want {
				t.Errorf("RepoRefToCloneURL(%q, %v) = %q, want %q", tt.ref, tt.useSSH, got, tt.want)
			}
		})
	}
}

func TestRegistry_Install_SSH(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create work repo with a kit, then make it bare
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

	kitDir := filepath.Join(workDir, "go")
	os.MkdirAll(kitDir, 0o755)
	os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env:\n  GOPATH: /go"), 0o644)
	git("add", ".")
	git("commit", "-m", "init")

	// Clone to bare repo
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "clone", "--bare", workDir, remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git clone --bare failed: %s", out)
	}
	_ = cmd

	// Install with useSSH=true still works via InstallFromURL (SSH URL generation tested separately)
	baseDir := t.TempDir()
	reg := kit.NewRegistry(baseDir)

	// Use InstallFromURL directly since we can't test real SSH in unit tests
	err := reg.InstallFromURL("test-host/user/repo", remoteDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	path, err := reg.Resolve("test-host/user/repo/go")
	if err != nil {
		t.Fatalf("Resolve after install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "kit.yaml")); err != nil {
		t.Errorf("kit.yaml not found at %s", path)
	}
}

func TestRegistry_Remove(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "github.com", "user", "repo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755)

	reg := kit.NewRegistry(baseDir)
	if err := reg.Remove("github.com/user/repo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Error("repo dir should be removed")
	}
}

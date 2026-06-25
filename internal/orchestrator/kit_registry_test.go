package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"

	kit "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestRegistry_Resolve(t *testing.T) {
	baseDir := t.TempDir()

	// Create a fake kit at baseDir/go-tools/kit.yaml
	kitDir := filepath.Join(baseDir, "go-tools")
	os.MkdirAll(kitDir, 0o755)
	os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env: {}"), 0o644)

	reg := kit.NewRegistry(baseDir)

	path, err := reg.Resolve("go-tools")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if path != kitDir {
		t.Errorf("path = %q, want %q", path, kitDir)
	}
}

func TestRegistry_Resolve_NotFound(t *testing.T) {
	reg := kit.NewRegistry(t.TempDir())
	_, err := reg.Resolve("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent kit")
	}
}

func TestRegistry_Resolve_InvalidName(t *testing.T) {
	reg := kit.NewRegistry(t.TempDir())
	// Old-style remote ref should be rejected by ValidKitName
	_, err := reg.Resolve("github.com/user/repo/go")
	if err == nil {
		t.Fatal("expected error for invalid kit name containing slashes")
	}
}

func TestRegistry_List(t *testing.T) {
	baseDir := t.TempDir()

	// Create fake kit dirs with kit.yaml
	for _, name := range []string{"go-tools", "node-lts"} {
		d := filepath.Join(baseDir, name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "kit.yaml"), []byte("env: {}"), 0o644)
	}
	// A dir without kit.yaml should be skipped
	os.MkdirAll(filepath.Join(baseDir, "incomplete"), 0o755)

	reg := kit.NewRegistry(baseDir)
	names, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("names = %v, want 2 entries", names)
	}
}

func TestRegistry_List_EmptyDir(t *testing.T) {
	reg := kit.NewRegistry(t.TempDir())
	names, err := reg.List()
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected no kits, got %v", names)
	}
}

func TestRegistry_List_MissingDir(t *testing.T) {
	reg := kit.NewRegistry("/tmp/boid-nonexistent-kits-dir-xyz-pr5")
	names, err := reg.List()
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected no kits, got %v", names)
	}
}

func TestRegistry_IsInstalled(t *testing.T) {
	baseDir := t.TempDir()
	kitDir := filepath.Join(baseDir, "go-tools")
	os.MkdirAll(kitDir, 0o755)

	reg := kit.NewRegistry(baseDir)

	if !reg.IsInstalled("go-tools") {
		t.Error("expected IsInstalled=true for existing dir")
	}
	if reg.IsInstalled("nonexistent") {
		t.Error("expected IsInstalled=false for missing dir")
	}
}

func TestRegistry_Remove(t *testing.T) {
	baseDir := t.TempDir()
	kitDir := filepath.Join(baseDir, "go-tools")
	os.MkdirAll(kitDir, 0o755)
	os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte("env: {}"), 0o644)

	reg := kit.NewRegistry(baseDir)
	if err := reg.Remove("go-tools"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(kitDir); !os.IsNotExist(err) {
		t.Error("kit dir should be removed")
	}
}

func TestRegistry_Remove_NotInstalled(t *testing.T) {
	reg := kit.NewRegistry(t.TempDir())
	if err := reg.Remove("nonexistent"); err == nil {
		t.Fatal("expected error removing nonexistent kit")
	}
}

func TestValidKitName(t *testing.T) {
	valid := []string{"go", "go-tools", "node-lts", "a", "123", "go123"}
	for _, s := range valid {
		if err := kit.ValidKitName(s); err != nil {
			t.Errorf("ValidKitName(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"Go",
		"go_tools",
		"go/tools",
		"go tools",
		"../etc",
		"github.com/user/repo",
	}
	for _, s := range invalid {
		if err := kit.ValidKitName(s); err == nil {
			t.Errorf("ValidKitName(%q) = nil, want error", s)
		}
	}

	// Too long (65 chars)
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if err := kit.ValidKitName(long); err == nil {
		t.Error("expected error for name exceeding 64 chars")
	}
}

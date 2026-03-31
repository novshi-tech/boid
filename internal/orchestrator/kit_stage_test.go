package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"

	kit "github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestStageHooks_ProjectOnly(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "build.sh"), []byte("#!/bin/bash\necho build"), 0o755)

	staged, cleanup, err := kit.StageHooks(projHooksDir, nil, "job-001")
	if err != nil {
		t.Fatalf("StageHooks: %v", err)
	}
	defer cleanup()

	content, err := os.ReadFile(filepath.Join(staged, "build.sh"))
	if err != nil {
		t.Fatalf("read staged build.sh: %v", err)
	}
	if string(content) != "#!/bin/bash\necho build" {
		t.Errorf("content = %q", content)
	}
}

func TestStageHooks_KitAndProject(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "proj-hook.sh"), []byte("project"), 0o755)

	kitHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(kitHooksDir, "kit-hook.sh"), []byte("kit"), 0o755)

	kitDirs := []projectspec.KitHooksInfo{
		{HooksDir: kitHooksDir, HookIDs: []string{"kit-hook"}},
	}

	staged, cleanup, err := kit.StageHooks(projHooksDir, kitDirs, "job-002")
	if err != nil {
		t.Fatalf("StageHooks: %v", err)
	}
	defer cleanup()

	// Both hooks should be present
	if _, err := os.Stat(filepath.Join(staged, "proj-hook.sh")); err != nil {
		t.Error("proj-hook.sh missing from staging")
	}
	if _, err := os.Stat(filepath.Join(staged, "kit-hook.sh")); err != nil {
		t.Error("kit-hook.sh missing from staging")
	}
}

func TestStageHooks_ProjectOverridesKit(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "build.sh"), []byte("project-version"), 0o755)

	kitHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(kitHooksDir, "build.sh"), []byte("kit-version"), 0o755)

	kitDirs := []projectspec.KitHooksInfo{
		{HooksDir: kitHooksDir, HookIDs: []string{"build"}},
	}

	staged, cleanup, err := kit.StageHooks(projHooksDir, kitDirs, "job-003")
	if err != nil {
		t.Fatalf("StageHooks: %v", err)
	}
	defer cleanup()

	content, err := os.ReadFile(filepath.Join(staged, "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "project-version" {
		t.Errorf("content = %q, want project-version (project should override kit)", string(content))
	}
}

func TestStageHooks_Cleanup(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "x.sh"), []byte("x"), 0o755)

	staged, cleanup, err := kit.StageHooks(projHooksDir, nil, "job-004")
	if err != nil {
		t.Fatal(err)
	}

	cleanup()

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staging dir should be removed after cleanup")
	}
}

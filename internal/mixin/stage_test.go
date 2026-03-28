package mixin_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/mixin"
	"github.com/novshi-tech/boid/internal/model"
)

func TestStageHooks_ProjectOnly(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "build.sh"), []byte("#!/bin/bash\necho build"), 0o755)

	staged, cleanup, err := mixin.StageHooks(projHooksDir, nil, "job-001")
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

func TestStageHooks_MixinAndProject(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "proj-hook.sh"), []byte("project"), 0o755)

	mixinHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(mixinHooksDir, "mixin-hook.sh"), []byte("mixin"), 0o755)

	mixinDirs := []model.MixinHooksInfo{
		{HooksDir: mixinHooksDir, HookIDs: []string{"mixin-hook"}},
	}

	staged, cleanup, err := mixin.StageHooks(projHooksDir, mixinDirs, "job-002")
	if err != nil {
		t.Fatalf("StageHooks: %v", err)
	}
	defer cleanup()

	// Both hooks should be present
	if _, err := os.Stat(filepath.Join(staged, "proj-hook.sh")); err != nil {
		t.Error("proj-hook.sh missing from staging")
	}
	if _, err := os.Stat(filepath.Join(staged, "mixin-hook.sh")); err != nil {
		t.Error("mixin-hook.sh missing from staging")
	}
}

func TestStageHooks_ProjectOverridesMixin(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "build.sh"), []byte("project-version"), 0o755)

	mixinHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(mixinHooksDir, "build.sh"), []byte("mixin-version"), 0o755)

	mixinDirs := []model.MixinHooksInfo{
		{HooksDir: mixinHooksDir, HookIDs: []string{"build"}},
	}

	staged, cleanup, err := mixin.StageHooks(projHooksDir, mixinDirs, "job-003")
	if err != nil {
		t.Fatalf("StageHooks: %v", err)
	}
	defer cleanup()

	content, err := os.ReadFile(filepath.Join(staged, "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "project-version" {
		t.Errorf("content = %q, want project-version (project should override mixin)", string(content))
	}
}

func TestStageHooks_Cleanup(t *testing.T) {
	projHooksDir := t.TempDir()
	os.WriteFile(filepath.Join(projHooksDir, "x.sh"), []byte("x"), 0o755)

	staged, cleanup, err := mixin.StageHooks(projHooksDir, nil, "job-004")
	if err != nil {
		t.Fatal(err)
	}

	cleanup()

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staging dir should be removed after cleanup")
	}
}

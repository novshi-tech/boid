package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestHookSurviveFullChain(t *testing.T) {
	// Set up a temp project directory
	dir := t.TempDir()

	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	os.MkdirAll(hooksDir, 0755)

	// Write project.yaml
	projectYAML := `id: test-project
name: Test Project
task_behaviors:
  parent:
    hooks:
      - id: spawn-parent
        traits:
          produces: [artifact]
    traits: []
  child:
    traits: []
`
	os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0644)

	// Write hook script
	spawnScript := "#!/usr/bin/env bash\necho spawning\n"
	os.WriteFile(filepath.Join(hooksDir, "spawn-parent.sh"), []byte(spawnScript), 0755)

	// ReadProjectMeta
	meta, err := orchestrator.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectMeta: %v", err)
	}
	parentB := meta.TaskBehaviors["parent"]
	t.Logf("After ReadProjectMeta: parent hooks = %d", len(parentB.Hooks))
	for _, h := range parentB.Hooks {
		t.Logf("  hook: id=%s scriptPath=%s", h.ID, h.ScriptPath)
	}
	if len(parentB.Hooks) == 0 {
		t.Fatal("FAIL: no hooks after ReadProjectMeta")
	}
	if parentB.Hooks[0].ScriptPath == "" {
		t.Fatal("FAIL: hook ScriptPath is empty after ReadProjectMeta")
	}

	// ReadProjectMetaWithKits
	meta2, err := orchestrator.ReadProjectMetaWithKits(dir, nil)
	if err != nil {
		t.Fatalf("ReadProjectMetaWithKits: %v", err)
	}
	parentB2 := meta2.TaskBehaviors["parent"]
	t.Logf("After ReadProjectMetaWithKits: parent hooks = %d", len(parentB2.Hooks))
	for _, h := range parentB2.Hooks {
		t.Logf("  hook: id=%s scriptPath=%s", h.ID, h.ScriptPath)
	}
	if len(parentB2.Hooks) == 0 {
		t.Fatal("FAIL: no hooks after ReadProjectMetaWithKits")
	}
	if parentB2.Hooks[0].ScriptPath == "" {
		t.Fatal("FAIL: hook ScriptPath is empty after ReadProjectMetaWithKits")
	}
}

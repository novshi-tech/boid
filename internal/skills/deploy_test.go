package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

func TestDeployAll_CreatesBothSkills(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}

	for _, skillName := range []string{"boid-sandbox", "boid-supervisor", "boid-executor", "boid-web", "boid-orchestrate", "boid-task"} {
		content, err := os.ReadFile(filepath.Join(baseDir, skillName, "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s/SKILL.md: %v", skillName, err)
		}
		if !strings.Contains(string(content), skillName) {
			t.Errorf("%s/SKILL.md missing skill name", skillName)
		}
	}

	for _, ref := range []string{"data-model.md"} {
		path := filepath.Join(baseDir, "boid-sandbox", "references", ref)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("boid-sandbox reference file missing: %s", ref)
		}
	}
}

func TestDeployAll_Idempotent(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}
	content1, _ := os.ReadFile(filepath.Join(baseDir, "boid-sandbox", "SKILL.md"))

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}
	content2, _ := os.ReadFile(filepath.Join(baseDir, "boid-sandbox", "SKILL.md"))

	if string(content1) != string(content2) {
		t.Error("idempotent deploy changed SKILL.md content")
	}
}

func TestDeployAll_UpdatesChangedFiles(t *testing.T) {
	baseDir := t.TempDir()

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (1st): %v", err)
	}

	stale := filepath.Join(baseDir, "boid-sandbox", "SKILL.md")
	if err := os.WriteFile(stale, []byte("old content"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll (2nd): %v", err)
	}

	content, _ := os.ReadFile(stale)
	if string(content) == "old content" {
		t.Error("DeployAll did not update stale SKILL.md")
	}
	if !strings.Contains(string(content), "boid-sandbox") {
		t.Error("updated SKILL.md missing expected content")
	}
}

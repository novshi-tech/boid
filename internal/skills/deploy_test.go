package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

func TestDeploy_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "boid-sandbox")

	if err := skills.Deploy(target); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// SKILL.md must exist
	skillContent, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if !strings.Contains(string(skillContent), "boid-sandbox") {
		t.Error("SKILL.md missing skill name")
	}

	// references must exist
	for _, ref := range []string{"state-machine.md", "data-model.md", "output-format.md"} {
		path := filepath.Join(target, "references", ref)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("reference file missing: %s", ref)
		}
	}
}

func TestDeploy_Idempotent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "boid-sandbox")

	if err := skills.Deploy(target); err != nil {
		t.Fatalf("Deploy (1st): %v", err)
	}

	content1, _ := os.ReadFile(filepath.Join(target, "SKILL.md"))

	if err := skills.Deploy(target); err != nil {
		t.Fatalf("Deploy (2nd): %v", err)
	}

	content2, _ := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if string(content1) != string(content2) {
		t.Error("idempotent deploy changed SKILL.md content")
	}
}

func TestDeploy_UpdatesChangedFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "boid-sandbox")

	if err := skills.Deploy(target); err != nil {
		t.Fatalf("Deploy (1st): %v", err)
	}

	// Write stale content
	stale := filepath.Join(target, "SKILL.md")
	if err := os.WriteFile(stale, []byte("old content"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if err := skills.Deploy(target); err != nil {
		t.Fatalf("Deploy (2nd): %v", err)
	}

	content, _ := os.ReadFile(stale)
	if string(content) == "old content" {
		t.Error("Deploy did not update stale SKILL.md")
	}
	if !strings.Contains(string(content), "boid-sandbox") {
		t.Error("updated SKILL.md missing expected content")
	}
}

package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
	"gopkg.in/yaml.v3"
)

// Skill discovery parses the YAML header before exposing a skill to an agent.
// Check the deployed files so malformed metadata cannot silently hide a skill.
func TestDeployedSkills_ValidFrontmatter(t *testing.T) {
	for _, set := range []struct {
		name   string
		deploy func(string) error
		names  []string
	}{
		{"sandbox", skills.DeployAll, skills.EmbeddedSkillNames()},
		{"host", skills.DeployHostSkills, skills.HostSkillNames()},
	} {
		t.Run(set.name, func(t *testing.T) {
			baseDir := t.TempDir()
			if err := set.deploy(baseDir); err != nil {
				t.Fatal(err)
			}
			for _, name := range set.names {
				t.Run(name, func(t *testing.T) {
					content, err := os.ReadFile(filepath.Join(baseDir, name, "SKILL.md"))
					if err != nil {
						t.Fatal(err)
					}
					header, ok := strings.CutPrefix(string(content), "---\n")
					if !ok {
						t.Fatal("missing opening frontmatter delimiter")
					}
					header, _, ok = strings.Cut(header, "\n---\n")
					if !ok {
						t.Fatal("missing closing frontmatter delimiter")
					}
					var metadata map[string]any
					if err := yaml.Unmarshal([]byte(header), &metadata); err != nil {
						t.Fatalf("invalid YAML frontmatter: %v", err)
					}
					if metadata["name"] != name {
						t.Errorf("name = %v, want %q", metadata["name"], name)
					}
					if description, ok := metadata["description"].(string); !ok || strings.TrimSpace(description) == "" {
						t.Error("description must be a nonempty string")
					}
				})
			}
		})
	}
}

package project

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/model"
	"gopkg.in/yaml.v3"
)

var validHookOnValues = map[string]bool{
	"pending":              true,
	"executing":            true,
	"verifying":            true,
	"in_review":            true,
	"collecting_feedback":  true,
	"done":                 true,
	"aborted":              true,
}

// ReadMeta reads and validates .boid/project.yaml from the given directory.
func ReadMeta(dir string) (*model.ProjectMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var meta model.ProjectMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}

	if meta.ID == "" {
		return nil, fmt.Errorf("project.yaml: id is required")
	}
	if meta.Name == "" {
		return nil, fmt.Errorf("project.yaml: name is required")
	}

	hooksDir := filepath.Join(dir, ".boid", "hooks")
	for i := range meta.Hooks {
		h := &meta.Hooks[i]
		if !validHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := resolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}

	return &meta, nil
}

func resolveHookScript(hooksDir, hookID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		p := filepath.Join(hooksDir, hookID+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("script not found: %s.(sh|py)", hookID)
}

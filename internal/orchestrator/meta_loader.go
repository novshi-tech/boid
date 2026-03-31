package orchestrator

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/project"
	"gopkg.in/yaml.v3"
)

// ReadProjectMeta reads and validates .boid/project.yaml from the given directory.
func ReadProjectMeta(dir string) (*project.ProjectMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var meta project.ProjectMeta
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
		if !project.ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := project.ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}

	gatesDir := filepath.Join(dir, ".boid", "gates")
	for i := range meta.Gates {
		g := &meta.Gates[i]
		if !project.ValidGateOnValues[g.On] {
			return nil, fmt.Errorf("gate %q: invalid on value %q", g.ID, g.On)
		}
		scriptPath, err := project.ResolveGateScript(gatesDir, g.ID)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", g.ID, err)
		}
		g.ScriptPath = scriptPath
	}

	return &meta, nil
}

func resolveKitRef(ref, projectDir string, resolver KitResolver) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) < 4 {
		localDir := filepath.Join(projectDir, ".boid", "kits", ref)
		yamlPath := filepath.Join(localDir, "kit.yaml")
		if _, err := os.Stat(yamlPath); err != nil {
			return "", fmt.Errorf("local kit %q: kit.yaml not found at %s", ref, localDir)
		}
		return localDir, nil
	}

	if resolver == nil {
		return "", fmt.Errorf("kit %q requires registry but none configured", ref)
	}
	return resolver.Resolve(ref)
}

// ReadProjectMetaWithKits reads project.yaml and resolves kit references.
func ReadProjectMetaWithKits(dir string, resolver KitResolver) (*project.ProjectMeta, error) {
	meta, err := ReadProjectMeta(dir)
	if err != nil {
		return nil, err
	}

	if len(meta.Kits) == 0 {
		return meta, nil
	}

	var kits []*project.KitMeta
	for _, ref := range meta.Kits {
		kitDir, err := resolveKitRef(ref, dir, resolver)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		kitMeta, err := ReadKitMeta(kitDir)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		slog.Info("resolved kit", "ref", ref, "hooks", len(kitMeta.Hooks))
		kits = append(kits, kitMeta)
	}

	return MergeKitMeta(meta, kits), nil
}

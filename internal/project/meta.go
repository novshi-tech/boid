package project

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/model"
	"gopkg.in/yaml.v3"
)

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
		if !model.ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := model.ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}

	return &meta, nil
}

// resolveKitRef resolves a kit reference to a filesystem directory.
// Single-segment refs (e.g. "go-dev") are resolved as local kits under .boid/kits/<ref>/.
// 4+ segment refs (e.g. "github.com/user/repo/kit") are resolved via the registry.
func resolveKitRef(ref, projectDir string, registry *kit.Registry) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) < 4 {
		// Local kit: .boid/kits/<ref>/
		localDir := filepath.Join(projectDir, ".boid", "kits", ref)
		yamlPath := filepath.Join(localDir, "kit.yaml")
		if _, err := os.Stat(yamlPath); err != nil {
			return "", fmt.Errorf("local kit %q: kit.yaml not found at %s", ref, localDir)
		}
		return localDir, nil
	}

	// Registry kit: 4+ segments
	if registry == nil {
		return "", fmt.Errorf("kit %q requires registry but none configured", ref)
	}
	return registry.Resolve(ref)
}

// ReadMetaWithKits reads project.yaml and resolves kit references.
// Local kits (single-segment refs) are resolved from .boid/kits/<ref>/.
// Registry kits (4+ segment refs) require a non-nil registry.
func ReadMetaWithKits(dir string, registry *kit.Registry) (*model.ProjectMeta, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return nil, err
	}

	if len(meta.Kits) == 0 {
		return meta, nil
	}

	var kits []*kit.KitMeta
	for _, ref := range meta.Kits {
		kitDir, err := resolveKitRef(ref, dir, registry)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		k, err := kit.ReadKit(kitDir)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		slog.Info("resolved kit", "ref", ref, "hooks", len(k.Hooks))
		kits = append(kits, k)
	}

	return kit.MergeKits(meta, kits), nil
}

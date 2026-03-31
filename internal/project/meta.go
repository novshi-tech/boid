package project

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReadMeta reads and validates .boid/project.yaml from the given directory.
func ReadMeta(dir string) (*ProjectMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var meta ProjectMeta
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
		if !ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}

	gatesDir := filepath.Join(dir, ".boid", "gates")
	for i := range meta.Gates {
		g := &meta.Gates[i]
		if !ValidGateOnValues[g.On] {
			return nil, fmt.Errorf("gate %q: invalid on value %q", g.ID, g.On)
		}
		scriptPath, err := ResolveGateScript(gatesDir, g.ID)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", g.ID, err)
		}
		g.ScriptPath = scriptPath
	}

	return &meta, nil
}

// resolveKitRef resolves a kit reference to a filesystem directory.
// Single-segment refs (e.g. "go-dev") resolve as local kits under .boid/kits/<ref>/.
// 4+ segment refs require a non-nil resolver.
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

// ReadMetaWithKits reads project.yaml and resolves kit references.
func ReadMetaWithKits(dir string, resolver KitResolver) (*ProjectMeta, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return nil, err
	}

	if len(meta.Kits) == 0 {
		return meta, nil
	}

	var kits []*KitMeta
	for _, ref := range meta.Kits {
		kitDir, err := resolveKitRef(ref, dir, resolver)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		k, err := ReadKit(kitDir)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		slog.Info("resolved kit", "ref", ref, "hooks", len(k.Hooks))
		kits = append(kits, k)
	}

	return MergeKits(meta, kits), nil
}

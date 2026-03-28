package project

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/mixin"
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

// ReadMetaWithMixins reads project.yaml and resolves mixin references.
// If registry is nil, behaves identically to ReadMeta.
func ReadMetaWithMixins(dir string, registry *mixin.Registry) (*model.ProjectMeta, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return nil, err
	}

	if registry == nil || len(meta.Mixins) == 0 {
		return meta, nil
	}

	var mixins []*mixin.MixinMeta
	for _, ref := range meta.Mixins {
		mixinDir, err := registry.Resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("mixin %q: %w", ref, err)
		}
		m, err := mixin.ReadMixin(mixinDir)
		if err != nil {
			return nil, fmt.Errorf("mixin %q: %w", ref, err)
		}
		slog.Info("resolved mixin", "ref", ref, "hooks", len(m.Hooks))
		mixins = append(mixins, m)
	}

	return mixin.MergeMixins(meta, mixins), nil
}

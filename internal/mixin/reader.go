package mixin

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/model"
	"gopkg.in/yaml.v3"
)

// ReadMixin reads and validates mixin.yaml from the given directory.
// Environment variables in string values are expanded using os.Expand.
func ReadMixin(dir string) (*MixinMeta, error) {
	yamlPath := filepath.Join(dir, "mixin.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read mixin.yaml: %w", err)
	}

	var m MixinMeta
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse mixin.yaml: %w", err)
	}

	// Interpolate environment variables
	interpolateEnvSlice(m.AdditionalBindings)
	interpolateEnvSlice(m.HostCommands)
	interpolateEnvSlice(m.AllowedDomains)
	interpolateEnvMap(m.Env)

	// Validate and resolve hooks
	hooksDir := filepath.Join(dir, "hooks")
	for i := range m.Hooks {
		h := &m.Hooks[i]
		if !model.ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := model.ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}

	if len(m.Hooks) > 0 {
		m.HooksDir = hooksDir
	}

	return &m, nil
}

func interpolateEnv(s string) string {
	return os.Expand(s, os.Getenv)
}

func interpolateEnvSlice(ss []string) {
	for i, s := range ss {
		ss[i] = interpolateEnv(s)
	}
}

func interpolateEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = interpolateEnv(v)
	}
}

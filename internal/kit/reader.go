package kit

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/model"
	"gopkg.in/yaml.v3"
)

// ReadKit reads and validates kit.yaml from the given directory.
// Environment variables in string values are expanded using os.Expand.
func ReadKit(dir string) (*KitMeta, error) {
	yamlPath := filepath.Join(dir, "kit.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read kit.yaml: %w", err)
	}

	var m KitMeta
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse kit.yaml: %w", err)
	}

	// Interpolate environment variables
	interpolateEnvSlice(m.AdditionalBindings)
	interpolateHostCommands(m.HostCommands)
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

func interpolateHostCommands(cmds map[string]hostcmd.CommandDef) {
	for name, def := range cmds {
		def.Path = interpolateEnv(def.Path)
		interpolateEnvMap(def.Env)
		cmds[name] = def
	}
}

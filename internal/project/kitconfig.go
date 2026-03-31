package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// KitMeta holds the parsed content of a kit.yaml file.
type KitMeta struct {
	TaskBehaviors      map[string]TaskBehavior `yaml:"task_behaviors"`
	Hooks              []Hook                  `yaml:"hooks"`
	Gates              []Gate                  `yaml:"gates"`
	HostCommands       map[string]CommandDef   `yaml:"host_commands"`
	AdditionalBindings []BindMount             `yaml:"additional_bindings"`
	Env                map[string]string       `yaml:"env"`

	// Set at load time, not from YAML.
	HooksDir string `yaml:"-"`
	GatesDir string `yaml:"-"`
}

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

	interpolateBindMounts(m.AdditionalBindings)
	interpolateHostCommands(m.HostCommands)
	interpolateEnvMap(m.Env)

	hooksDir := filepath.Join(dir, "hooks")
	for i := range m.Hooks {
		h := &m.Hooks[i]
		if !ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}
	if len(m.Hooks) > 0 {
		m.HooksDir = hooksDir
	}

	gatesDir := filepath.Join(dir, "gates")
	for i := range m.Gates {
		g := &m.Gates[i]
		if !ValidGateOnValues[g.On] {
			return nil, fmt.Errorf("gate %q: invalid on value %q", g.ID, g.On)
		}
		scriptPath, err := ResolveGateScript(gatesDir, g.ID)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", g.ID, err)
		}
		g.ScriptPath = scriptPath
	}
	if len(m.Gates) > 0 {
		m.GatesDir = gatesDir
	}

	return &m, nil
}

// MergeKits merges kit configurations into a base ProjectMeta.
// Kits are applied in order; project values take precedence.
func MergeKits(base *ProjectMeta, kits []*KitMeta) *ProjectMeta {
	if len(kits) == 0 {
		return base
	}

	result := *base

	mergedEnv := make(map[string]string)
	for _, m := range kits {
		for k, v := range m.Env {
			mergedEnv[k] = v
		}
	}
	for k, v := range base.Env {
		mergedEnv[k] = v
	}
	result.Env = mergedEnv

	mergedBehaviors := make(map[string]TaskBehavior)
	for _, m := range kits {
		for k, v := range m.TaskBehaviors {
			mergedBehaviors[k] = v
		}
	}
	for k, v := range base.TaskBehaviors {
		mergedBehaviors[k] = v
	}
	result.TaskBehaviors = mergedBehaviors

	var allHooks []Hook
	for _, m := range kits {
		allHooks = append(allHooks, m.Hooks...)
	}
	allHooks = append(allHooks, base.Hooks...)
	result.Hooks = dedupHooks(allHooks)

	var allGates []Gate
	for _, m := range kits {
		allGates = append(allGates, m.Gates...)
	}
	allGates = append(allGates, base.Gates...)
	result.Gates = dedupGates(allGates)

	mergedCmds := make(map[string]CommandDef)
	for _, m := range kits {
		for k, v := range m.HostCommands {
			mergedCmds[k] = v
		}
	}
	for k, v := range base.HostCommands {
		mergedCmds[k] = v
	}
	if len(mergedCmds) > 0 {
		result.HostCommands = mergedCmds
	}

	result.AdditionalBindings = unionBindMounts(kits, base.AdditionalBindings)

	for _, m := range kits {
		if m.HooksDir == "" || len(m.Hooks) == 0 {
			continue
		}
		ids := make([]string, len(m.Hooks))
		for i, h := range m.Hooks {
			ids[i] = h.ID
		}
		result.KitHooksDirs = append(result.KitHooksDirs, KitHooksInfo{
			HooksDir: m.HooksDir,
			HookIDs:  ids,
		})
	}

	for _, m := range kits {
		if m.GatesDir == "" || len(m.Gates) == 0 {
			continue
		}
		ids := make([]string, len(m.Gates))
		for i, g := range m.Gates {
			ids[i] = g.ID
		}
		result.KitGatesDirs = append(result.KitGatesDirs, KitGatesInfo{
			GatesDir: m.GatesDir,
			GateIDs:  ids,
		})
	}

	return &result
}

func dedupHooks(hooks []Hook) []Hook {
	seen := make(map[string]int)
	var result []Hook
	for _, h := range hooks {
		if idx, ok := seen[h.ID]; ok {
			result[idx] = h
		} else {
			seen[h.ID] = len(result)
			result = append(result, h)
		}
	}
	return result
}

func dedupGates(gates []Gate) []Gate {
	seen := make(map[string]int)
	var result []Gate
	for _, g := range gates {
		if idx, ok := seen[g.ID]; ok {
			result[idx] = g
		} else {
			seen[g.ID] = len(result)
			result = append(result, g)
		}
	}
	return result
}

func unionBindMounts(kits []*KitMeta, base []BindMount) []BindMount {
	seen := make(map[string]bool)
	var result []BindMount
	for _, m := range kits {
		for _, b := range m.AdditionalBindings {
			if !seen[b.Source] {
				seen[b.Source] = true
				result = append(result, b)
			}
		}
	}
	for _, b := range base {
		if !seen[b.Source] {
			seen[b.Source] = true
			result = append(result, b)
		}
	}
	return result
}

func interpolateEnv(s string) string {
	return os.Expand(s, os.Getenv)
}

func interpolateBindMounts(mounts []BindMount) {
	for i := range mounts {
		mounts[i].Source = interpolateEnv(mounts[i].Source)
	}
}

func interpolateEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = interpolateEnv(v)
	}
}

func interpolateHostCommands(cmds map[string]CommandDef) {
	for name, def := range cmds {
		def.Path = interpolateEnv(def.Path)
		interpolateEnvMap(def.Env)
		cmds[name] = def
	}
}

// StageHooks creates a temporary directory containing all hook scripts
// from the project and all kits. Project scripts override kit scripts.
// Returns the staging directory path and a cleanup function.
func StageHooks(projectHooksDir string, kitHooksDirs []KitHooksInfo, jobID string) (string, func(), error) {
	stagingDir := filepath.Join(os.TempDir(), fmt.Sprintf("boid-hooks-%s", jobID))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(stagingDir)
	}

	for _, m := range kitHooksDirs {
		if err := copyHookScripts(m.HooksDir, stagingDir); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy kit hooks from %s: %w", m.HooksDir, err)
		}
	}

	if err := copyHookScripts(projectHooksDir, stagingDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy project hooks: %w", err)
	}

	return stagingDir, cleanup, nil
}

func copyHookScripts(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".sh" && ext != ".py" {
			continue
		}
		if err := copyFile(filepath.Join(srcDir, e.Name()), filepath.Join(dstDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

package orchestrator

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func validateWorkspaceDefaultTaskBehaviors(scope string, behaviors map[string]TaskBehavior) (map[string]TaskBehavior, error) {
	if len(behaviors) == 0 {
		return behaviors, nil
	}
	for name, behavior := range behaviors {
		for i := range behavior.Hooks {
			if err := validateHookKind(&behavior.Hooks[i]); err != nil {
				return nil, fmt.Errorf("%s: task_behaviors.%s: %w", scope, name, err)
			}
		}
	}
	return behaviors, nil
}

func validateHookKind(h *Hook) error {
	if !h.Kind.IsValid() {
		return fmt.Errorf("hook %q: invalid kind %q (allowed: \"\" or \"agent\")", h.ID, h.Kind)
	}
	if h.Kind != HandlerKindAgent && h.Agent != "" {
		return fmt.Errorf("hook %q: 'agent' field requires 'kind: agent' (non-agent hooks must not declare agent)", h.ID)
	}
	if h.Kind == HandlerKindAgent && h.Command != "" {
		return fmt.Errorf("hook %q: kind %q does not allow 'command:' (agent hooks are dispatched to a HarnessAdapter, which builds its own argv)", h.ID, h.Kind)
	}
	return nil
}

func mergeBindMounts(base, overlay []BindMount) []BindMount {
	if len(overlay) == 0 {
		return cloneBindMounts(base)
	}

	result := cloneBindMounts(base)
	indexBySource := make(map[string]int, len(result))
	for i, binding := range result {
		indexBySource[binding.Source] = i
	}

	for _, binding := range overlay {
		if idx, ok := indexBySource[binding.Source]; ok {
			result[idx] = binding
			continue
		}
		indexBySource[binding.Source] = len(result)
		result = append(result, binding)
	}
	return result
}

func interpolateEnv(s string) string {
	return os.Expand(s, os.Getenv)
}

func interpolateEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = interpolateEnv(v)
	}
}

func interpolateHostCommands(cmds HostCommands) {
	for name, spec := range cmds {
		spec.Path = interpolateEnv(spec.Path)
		interpolateHostCommandEnvMap(spec.Env)
		cmds[name] = spec
	}
}

func interpolateHostCommandEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = os.Expand(v, func(name string) string {
			if strings.HasPrefix(name, "boid:") {
				return "${" + name + "}"
			}
			return os.Getenv(name)
		})
	}
}

func cloneProjectMeta(meta *ProjectMeta) *ProjectMeta {
	if meta == nil {
		return nil
	}

	result := *meta
	result.Env = mergeStringMaps(nil, meta.Env)
	result.HostCommands = cloneHostCommands(meta.HostCommands)
	result.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	result.TaskBehaviors = cloneTaskBehaviorMap(meta.TaskBehaviors)
	result.CardCommands = cloneCardCommandMap(meta.CardCommands)
	result.CardCommandsOrder = append([]string(nil), meta.CardCommandsOrder...)
	return &result
}

func cloneCardCommandMap(src map[string]CardCommand) map[string]CardCommand {
	if len(src) == 0 {
		return nil
	}
	result := make(map[string]CardCommand, len(src))
	for k, v := range src {
		result[k] = v
	}
	return result
}

func cloneTaskBehaviorMap(src map[string]TaskBehavior) map[string]TaskBehavior {
	if len(src) == 0 {
		return nil
	}
	result := make(map[string]TaskBehavior, len(src))
	for k, v := range src {
		if len(v.Hooks) > 0 {
			hooks := make([]Hook, len(v.Hooks))
			copy(hooks, v.Hooks)
			v.Hooks = hooks
		}
		v.Env = nil
		v.HostCommands = nil
		v.AdditionalBindings = nil
		result[k] = v
	}
	return result
}

func cloneHostCommands(cmds HostCommands) HostCommands {
	return mergeHostCommands(nil, cmds)
}

func cloneBindMounts(mounts []BindMount) []BindMount {
	if len(mounts) == 0 {
		return nil
	}
	result := make([]BindMount, len(mounts))
	copy(result, mounts)
	return result
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	result := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		result[k] = v
	}
	return result
}

func mergeHostCommands(base, overlay HostCommands) HostCommands {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	result := make(HostCommands, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		result[k] = v
	}
	return result
}

func validateBuiltinHostConflict(scope string, hostCommands HostCommands) error {
	for _, name := range []string{"git", "boid", "fetch"} {
		if _, conflict := hostCommands[name]; conflict {
			return fmt.Errorf("%s: %q is a reserved builtin/sandbox name and cannot be declared in host_commands", scope, name)
		}
	}
	return nil
}

func validateRejectRules(hostCommands HostCommands) error {
	names := make([]string, 0, len(hostCommands))
	for name := range hostCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, rule := range hostCommands[name].Reject {
			if rule.Match == "" {
				return fmt.Errorf("host_commands.%s.reject[%d]: match is required", name, i)
			}
			if rule.Reason == "" {
				return fmt.Errorf("host_commands.%s.reject[%d]: reason is required", name, i)
			}
		}
	}
	return nil
}

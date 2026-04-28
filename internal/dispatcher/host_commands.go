package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ResolveHostCommands turns the orchestrator-side host command map (keyed by
// the user-declared name) into a map keyed by the absolute path that the
// boid shim will be bind-mounted at inside the sandbox. The absolute path is
// also written back into each entry's Path so the broker spawns the right
// binary on the host without a second lookup.
//
// The same map is used both as the broker's policy table and as the source
// of shim mount targets; sharing a single resolved view guarantees that the
// `os.Executable()` value the shim sends will match a key the broker holds.
//
// Builtins ("boid", "git") are excluded — they have dedicated mounts and
// builtin policies elsewhere.
//
// `projectDir` is used to resolve relative paths declared in
// host_commands.<name>.path. `lookPath` is parameterized for tests; production
// callers pass exec.LookPath.
func ResolveHostCommands(
	builtins []string,
	hostCommands map[string]orchestrator.CommandDef,
	projectDir string,
	lookPath func(string) (string, error),
) (map[string]orchestrator.CommandDef, error) {
	out := make(map[string]orchestrator.CommandDef)

	for _, name := range builtins {
		if name == "boid" || name == "git" {
			continue
		}
		if _, ok := hostCommands[name]; ok {
			continue
		}
		absPath, err := lookPath(name)
		if err != nil {
			return nil, fmt.Errorf("host command %q not found on host: %w", name, err)
		}
		if _, dup := out[absPath]; dup {
			continue
		}
		out[absPath] = orchestrator.CommandDef{Name: name, Path: absPath}
	}

	for name, def := range hostCommands {
		if name == "boid" || name == "git" {
			continue
		}
		var absPath string
		if def.Path != "" {
			p := def.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(projectDir, p)
			}
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("host_commands.%s.path %q does not exist on host", name, def.Path)
			}
			absPath = p
		} else {
			p, err := lookPath(name)
			if err != nil {
				return nil, fmt.Errorf("host command %q not found on host: %w", name, err)
			}
			absPath = p
		}
		if _, dup := out[absPath]; dup {
			continue
		}
		cd := def
		cd.Name = name
		cd.Path = absPath
		out[absPath] = cd
	}

	return out, nil
}

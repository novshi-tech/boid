package orchestrator

import (
	"os"
	"path/filepath"
	"sort"
)

// ValidateHostCommandsInstalled resolves every configured host_commands
// entry against the daemon's own host. Since the daemon runs in a
// container, the "host" a host command resolves against IS the daemon
// image — an image missing a tool like `gh` would silently break every
// dispatch that uses it unless something catches the gap before the first
// affected job runs. This is that check.
//
// Returns the sorted list of configured command names that could not be
// resolved. This is advisory only: a missing command is reported by the
// caller (typically via slog.Warn — see internal/server.Server.New's call
// site), not treated as fatal. A missing command already fails lazily
// per-dispatch (ResolveHostCommands, host_commands.go); this check only
// surfaces the gap earlier, at boot.
//
// Commands declared with an explicit relative Path (project-local scripts)
// are skipped, not reported missing: their real location depends on a
// project's own checkout directory, which this daemon-wide, project-agnostic
// pass has no access to (ResolveHostCommands already validates those
// against the right projectDir at dispatch time). An absolute Path is
// checked directly via os.Stat; an empty Path (the common case — the
// command is expected to already be on the daemon's PATH) is checked via lookPath.
func ValidateHostCommandsInstalled(hostCommands map[string]HostCommandSpec, lookPath func(string) (string, error)) []string {
	names := make([]string, 0, len(hostCommands))
	for name := range hostCommands {
		names = append(names, name)
	}
	sort.Strings(names)

	var missing []string
	for _, name := range names {
		spec := hostCommands[name]
		switch {
		case spec.Path == "":
			if _, err := lookPath(name); err != nil {
				missing = append(missing, name)
			}
		case filepath.IsAbs(spec.Path):
			if _, err := os.Stat(spec.Path); err != nil {
				missing = append(missing, name)
			}
		default:
			// Relative Path: a project-local script. Not resolvable without
			// a specific project's checkout directory — intentionally
			// skipped here (see the doc comment above).
		}
	}
	return missing
}

package orchestrator

import (
	"os"
	"path/filepath"
	"sort"
)

// ValidateHostCommandsInstalled resolves every configured host_commands
// entry against the daemon's own host (docs/plans/phase6-container-backend.md
// §PR6, §決定4: "broker が exec する host command の実体... host_commands
// config の全 command 解決を daemon 起動時に検証する"). Once the daemon runs
// in a container (compose deploy, §決定4's DooD model), the "host" a host
// command resolves against IS the daemon image — an image built with "OS
// 土台 + boid のみ" would silently break every dispatch that uses a tool
// like `gh` unless something catches the gap before the first affected job
// runs. This is that check.
//
// Returns the sorted list of configured command names that could not be
// resolved. This is advisory only (skeleton level, PR6): a missing command
// is reported by the caller (typically via slog.Warn — see
// internal/server.Server.New's call site), not treated as fatal. Making
// this fail-closed (refuse to start) is a future knob; today a missing
// command already fails lazily per-dispatch (ResolveHostCommands,
// host_commands.go), which this check does not change — it only surfaces
// the gap earlier, at boot, rather than the first time an affected project
// dispatches.
//
// Commands declared with an explicit *relative* Path (project-local
// scripts — §決定4: "project-local script は project 側から提供") are
// skipped, not reported missing: their real location depends on a
// project's own checkout directory, which this daemon-wide,
// project-agnostic pass has no access to (ResolveHostCommands already
// validates those against the right projectDir at dispatch time). An
// absolute Path is checked directly via os.Stat; an empty Path (the common
// case — the command is expected to already be on the daemon's PATH, e.g.
// a provisioned tool like `gh`) is checked via lookPath.
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

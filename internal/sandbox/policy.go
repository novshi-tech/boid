package sandbox

import (
	"fmt"
	"strings"
)

// CommandDef is the canonical sandbox-side policy for brokered host commands.
// Dispatcher and orchestrator mirror this shape as transport data, but the
// policy semantics live here.
type CommandDef struct {
	Name               string
	Path               string
	AllowedPatterns    []string
	DeniedPatterns     []string
	AllowedSubcommands []string
	AllowStdin         bool
	Env                map[string]string
	// MissingSecrets lists "ENV_NAME (secret:key)" entries for env vars that
	// the kit declared via the "secret:" prefix but failed to resolve at
	// register time. When non-empty, the broker rejects exec requests for
	// this command (fail-closed) instead of silently letting host env /
	// host-side config fallbacks take over.
	MissingSecrets []string
}

// MissingSecretsMessage returns the human-readable rejection message used when
// a host_command is invoked despite unresolved declared secrets. Returns an
// empty string if MissingSecrets is empty.
func (d CommandDef) MissingSecretsMessage() string {
	if len(d.MissingSecrets) == 0 {
		return ""
	}
	return fmt.Sprintf("host_commands.%s: rejected — required secret(s) unavailable: %s", d.Name, strings.Join(d.MissingSecrets, ", "))
}

func CheckPolicy(def CommandDef, args []string) bool {
	if len(args) == 0 {
		return true
	}

	joined := strings.Join(args, " ")

	subcmdPassed := false
	if len(def.AllowedSubcommands) > 0 {
		subcmd := extractSimpleSubcommand(args)
		if subcmd != "" && !containsString(def.AllowedSubcommands, subcmd) {
			return false
		}
		subcmdPassed = true
	}

	if matchesAnyPattern(def.DeniedPatterns, joined) {
		return false
	}

	if matchesAnyPattern(def.AllowedPatterns, joined) {
		return true
	}

	if subcmdPassed && len(def.AllowedPatterns) == 0 {
		return true
	}

	return false
}

func matchesAnyPattern(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			pattern = pattern[1:]
			s = s[1:]
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
			pattern = pattern[1:]
			s = s[1:]
		}
	}
	return len(s) == 0
}

func extractSimpleSubcommand(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}


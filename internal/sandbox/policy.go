package sandbox

import "strings"

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
}

func CheckPolicy(def CommandDef, args []string) bool {
	if len(args) == 0 {
		return true
	}

	joined := strings.Join(args, " ")

	subcmdPassed := false
	if len(def.AllowedSubcommands) > 0 {
		subcmd := extractSimpleSubcommand(args)
		if subcmd == "" || !containsString(def.AllowedSubcommands, subcmd) {
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


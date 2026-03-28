package hostcmd

import "path/filepath"

type CommandDef struct {
	Name            string
	Path            string
	AllowedPatterns []string
	AllowStdin      bool
	Env             map[string]string
}

func CheckPolicy(def CommandDef, args []string) bool {
	for _, arg := range args {
		if !matchesAnyPattern(def.AllowedPatterns, arg) {
			return false
		}
	}
	return true
}

func matchesAnyPattern(patterns []string, arg string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, arg); matched {
			return true
		}
	}
	return false
}

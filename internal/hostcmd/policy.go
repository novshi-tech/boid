package hostcmd

import "strings"

type CommandDef struct {
	Name                string            `yaml:"name" json:"name"`
	Path                string            `yaml:"path" json:"path"`
	AllowedPatterns     []string          `yaml:"allowed_patterns" json:"allowed_patterns"`
	DeniedPatterns      []string          `yaml:"denied_patterns" json:"denied_patterns"`
	AllowedSubcommands  []string          `yaml:"allowed_subcommands" json:"allowed_subcommands"`
	AllowStdin          bool              `yaml:"allow_stdin" json:"allow_stdin"`
	Env                 map[string]string `yaml:"env" json:"env"`
	ExtractSubcommandFn string            `yaml:"extract_subcommand_fn" json:"extract_subcommand_fn"`
	RequireCwd          bool              `yaml:"require_cwd" json:"require_cwd"`
	AllowedCwdPrefixes  []string          `yaml:"allowed_cwd_prefixes" json:"allowed_cwd_prefixes"`
}

// CheckPolicy evaluates whether the given args are allowed for the command.
// Evaluation order:
//  1. AllowedSubcommands — if set, extract subcommand and check whitelist
//  2. DeniedPatterns — if any match the joined args, deny
//  3. AllowedPatterns — if any match the joined args, allow
//  4. Default deny
func CheckPolicy(def CommandDef, args []string) bool {
	if len(args) == 0 {
		return true
	}

	joined := strings.Join(args, " ")

	// 1. Subcommand whitelist
	if len(def.AllowedSubcommands) > 0 {
		var subcmd string
		if def.ExtractSubcommandFn == "git" {
			subcmd = extractGitSubcommand(args)
		} else {
			subcmd = extractSimpleSubcommand(args)
		}
		if subcmd == "" || !containsString(def.AllowedSubcommands, subcmd) {
			return false
		}
	}

	// 2. Deny-first: check denied patterns against joined args
	if matchesAnyPattern(def.DeniedPatterns, joined) {
		return false
	}

	// 3. Check allowed patterns against joined args
	if matchesAnyPattern(def.AllowedPatterns, joined) {
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

// globMatch performs simple glob matching where * matches any characters
// (including / and spaces), unlike filepath.Match.
func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Trim consecutive stars
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true // trailing * matches everything
			}
			// Try matching rest of pattern at every position
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

// extractSimpleSubcommand returns the first non-flag argument.
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

// extractGitSubcommand extracts the git subcommand from args,
// skipping global options like -C, -c, --git-dir, etc.
func extractGitSubcommand(args []string) string {
	// Global options that take a value argument
	valueOpts := map[string]bool{
		"-C":          true,
		"-c":          true,
		"--git-dir":   true,
		"--work-tree": true,
		"--namespace":  true,
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		if valueOpts[arg] {
			i += 2 // skip option and its value
			continue
		}
		// Handle --option=value style
		if strings.HasPrefix(arg, "--git-dir=") ||
			strings.HasPrefix(arg, "--work-tree=") ||
			strings.HasPrefix(arg, "--namespace=") {
			i++
			continue
		}
		// Skip other flags (e.g., --bare, --no-pager)
		if strings.HasPrefix(arg, "-") {
			i++
			continue
		}
		return arg
	}
	return ""
}

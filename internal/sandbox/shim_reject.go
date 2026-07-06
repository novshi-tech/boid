package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// HostCommandRulesEnv is the name of the env var the dispatcher injects with
// a compact JSON map of command name -> reject rules
// (see internal/dispatcher/sandbox_builder.go). The shim reads it to reject
// obviously-doomed invocations before paying for a broker round trip. The
// broker remains the authority: this is a UX fast path only, and any parse
// failure or non-match here simply falls through to the broker check.
const HostCommandRulesEnv = "BOID_HOST_COMMAND_RULES"

// EarlyReject checks command/args against the reject rules encoded in raw
// (the BOID_HOST_COMMAND_RULES JSON payload) and returns the same rejection
// message the broker would produce, plus true, on the first matching rule.
// Malformed JSON, an absent command entry, or no matching rule all yield
// ("", false) so callers fall through to the broker's own check.
func EarlyReject(raw, command string, args []string) (string, bool) {
	if raw == "" || command == "" {
		return "", false
	}

	var rules map[string][]RejectRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return "", false
	}

	commandRules, ok := rules[command]
	if !ok || len(commandRules) == 0 {
		return "", false
	}

	joined := strings.Join(args, " ")
	for _, rule := range commandRules {
		if globMatch(rule.Match, joined) {
			return fmt.Sprintf("host_commands.%s: rejected: %s", command, rule.Reason), true
		}
	}
	return "", false
}

// EarlyRejectFromEnv is the thin env-reading wrapper around EarlyReject used
// by shimMain.
func EarlyRejectFromEnv(command string, args []string) (string, bool) {
	return EarlyReject(os.Getenv(HostCommandRulesEnv), command, args)
}

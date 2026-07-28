package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// LogLevelNames is the accepted set of config.yaml log.level values, in the
// same order schema.go's "log.level" KindEnum leaf exposes as its
// EnumValues (so `boid config set log.level <bogus>`'s error message and
// this package's own ParseLogLevel error message list the exact same
// options — see TestLogLevelNames_MatchesParseLogLevel).
var LogLevelNames = []string{"debug", "info", "warn", "error"}

// ParseLogLevel maps a config.yaml log.level string to its slog.Level
// constant. It is the single source of truth both Config.UnmarshalYAML
// (load-time validation — an unrecognized string is a hard config error,
// same treatment as an unrecognized gateway.forges.*.forge value) and the
// daemon's own startup wiring (internal/daemon.ApplyLogLevel, which calls
// slog.SetLogLoggerLevel with the result) consult, so the two can never
// silently disagree about which strings are valid.
func ParseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want one of %s)", s, strings.Join(LogLevelNames, ", "))
	}
}

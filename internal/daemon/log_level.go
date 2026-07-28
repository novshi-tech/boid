package daemon

import (
	"log/slog"

	"github.com/novshi-tech/boid/internal/config"
)

// ApplyLogLevel sets the daemon process's slog output level from
// config.yaml's log.level, called once from cmd/start.go's runDaemonChild
// right after the log file redirect is in place — before anything else in
// the daemon's startup path emits a log line, so the whole process lifetime
// observes one consistent level.
//
// It works entirely by calling the standard library's
// slog.SetLogLoggerLevel, WITHOUT installing a slog.Handler (no
// slog.SetDefault call anywhere in this function or its callers) — see
// internal/config.LogConfig's doc comment for the full rationale. Nothing
// in the daemon ever calls slog.SetDefault, so every slog.Info/Debug/Warn/
// Error call still goes through slog's package-private defaultHandler,
// which is the ONE thing that decides boid.log's line format
// ("2009/11/10 23:00:00 INFO msg key=value"). SetLogLoggerLevel only moves
// the threshold that handler's Enabled check compares against; it does not
// touch how a passing record gets formatted. That is what makes this
// mechanism safe to add without altering the format every existing
// boid.log-grepping runbook depends on.
//
// level == "" (config.yaml has no log.level, i.e. config.LogConfig's zero
// value) is a deliberate no-op: slog's own built-in default (info) stands,
// exactly reproducing every pre-this-function daemon's behavior.
//
// A non-empty, unrecognized level should be unreachable in practice —
// config.Load (via Config.UnmarshalYAML) already runs the same
// config.ParseLogLevel check at config-load time and fails the whole daemon
// startup on a bad value (see internal/config's
// TestLoadFromPath_LogLevel_InvalidRejected) — but ApplyLogLevel still
// checks the error defensively rather than assuming it. Reaching this
// branch anyway does not abort startup: RedirectToLogRotating has already
// swapped stdout/stderr to the log file by the time cmd/start.go calls
// this, so failing loudly here would just be a different, later way to
// crash an otherwise-healthy daemon over what would be a config-package
// bug, not a bad user config. It logs a warning and leaves the level at
// whatever it already was (the safe, current-behavior fallback) instead.
func ApplyLogLevel(level string) {
	if level == "" {
		return
	}
	lvl, err := config.ParseLogLevel(level)
	if err != nil {
		slog.Warn("ignoring invalid log.level (config.Load should have already rejected this at daemon startup)",
			"value", level, "error", err)
		return
	}
	slog.SetLogLoggerLevel(lvl)
}

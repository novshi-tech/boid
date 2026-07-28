package daemon

import (
	"log/slog"

	"github.com/novshi-tech/boid/internal/config"
)

// ApplyLogLevel sets the daemon process's slog output level from
// config.yaml's log.level, called once from cmd/start.go's runDaemonChild
// right after the log file redirect is in place — the first slog-affecting
// call WITHIN runDaemonChild, so everything that function itself logs (the
// BOID_CLI_TOKEN warning, "boid server started", ...) observes the
// configured level. This is not quite "before anything else in the whole
// process": runStart's own earlier config.Load() call (buildStartConfig,
// in the same process, before runDaemonChild is even invoked) can itself
// emit a couple of slog.Warn lines (the deprecated sandbox.backend /
// gateway.hosts warnings, internal/config/config.go) — those land on
// whatever stdout/stderr this process inherited, before
// RedirectToLogRotating swaps it to the log file, and before any log.level
// value could apply to them even if it wanted to. In practice this is a
// cosmetic gap (those two warnings fire regardless of log.level, and no
// slog.Debug call sits in that earlier window today), not a correctness
// one.
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

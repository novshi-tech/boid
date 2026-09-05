package daemon

import (
	"log/slog"

	"github.com/novshi-tech/boid/internal/config"
)

// ApplyLogLevel sets the daemon process's slog output level from
// config.yaml's log.level, called once from cmd/start.go's runDaemonChild
// right after the log file redirect is in place.
//
// It calls only slog.SetLogLoggerLevel, never slog.SetDefault — see
// internal/config.LogConfig's doc comment for why: that keeps boid.log's
// line format untouched, since SetLogLoggerLevel only moves the threshold
// slog's default handler compares against, not how a record is formatted.
//
// level == "" is a deliberate no-op (slog's built-in default level stands).
// An unrecognized level should be unreachable — config.Load already
// validates it at startup — but is handled defensively with a warning
// rather than aborting an otherwise-healthy daemon.
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

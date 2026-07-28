package daemon

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

// captureStdLog redirects the standard "log" package's output — which is
// exactly what slog's package-private defaultHandler writes through, as
// long as nothing calls slog.SetDefault (see ApplyLogLevel's own doc
// comment for why that invariant matters) — into an in-memory buffer for
// the duration of the test, restoring both the previous writer and slog's
// log-package bridge level (reset to its zero-value default, LevelInfo) on
// cleanup so tests in this file cannot leak global state into each other or
// into any other test in this package's binary.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags)
	slog.SetLogLoggerLevel(slog.LevelInfo)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		slog.SetLogLoggerLevel(slog.LevelInfo)
	})
	return &buf
}

// stdLogLineRE pins boid.log's CURRENT line format
// ("2009/11/10 23:00:00 INFO msg key=value" — date, time, level word,
// message, space-separated key=value attrs) — see internal/config's
// LogConfig doc comment for why a log.level knob must not change this. A
// TextHandler/JSONHandler line ("time=... level=INFO msg=...") would not
// match this regex at all, which is exactly the mutation
// TestApplyLogLevel_LineFormatUnchanged exists to catch.
var stdLogLineRE = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (DEBUG|INFO|WARN|ERROR) .+$`)

// TestApplyLogLevel_LineFormatUnchanged is the format-pin test called out by
// the task: it must fail if boid.log's line shape ever changes (e.g. a
// future change swaps in slog.NewTextHandler/JSONHandler instead of the
// bridge-level-only ApplyLogLevel implements). See the PR body for the
// mutation-proof run against a TextHandler-based reimplementation.
func TestApplyLogLevel_LineFormatUnchanged(t *testing.T) {
	buf := captureStdLog(t)

	ApplyLogLevel("debug") // enable both lines below for this one assertion
	slog.Info("hello world", "key", "value")
	slog.Debug("debug line", "key", "value")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d output line(s), want 2:\n%s", len(lines), buf.String())
	}
	for _, line := range lines {
		if !stdLogLineRE.MatchString(line) {
			t.Errorf("line %q does not match boid.log's current format (regex %s)", line, stdLogLineRE.String())
		}
	}
	if !strings.Contains(lines[0], "INFO hello world key=value") {
		t.Errorf("INFO line = %q, want it to contain %q", lines[0], "INFO hello world key=value")
	}
	if !strings.Contains(lines[1], "DEBUG debug line key=value") {
		t.Errorf("DEBUG line = %q, want it to contain %q", lines[1], "DEBUG debug line key=value")
	}
}

// TestApplyLogLevel_Debug_EnablesDebugOutput pins that log.level: debug
// actually lets slog.Debug calls through.
func TestApplyLogLevel_Debug_EnablesDebugOutput(t *testing.T) {
	buf := captureStdLog(t)
	ApplyLogLevel("debug")

	slog.Debug("should appear", "k", "v")
	if !strings.Contains(buf.String(), "DEBUG should appear") {
		t.Errorf("expected a DEBUG line in output, got: %q", buf.String())
	}
}

// TestApplyLogLevel_Empty_KeepsDefault_NoDebugOutput pins that an unset
// log.level ("" — config.yaml has no log.level, or ApplyLogLevel is never
// called at all) reproduces exactly today's pre-this-feature behavior:
// slog.Debug produces nothing, slog.Info still works.
func TestApplyLogLevel_Empty_KeepsDefault_NoDebugOutput(t *testing.T) {
	buf := captureStdLog(t)
	ApplyLogLevel("")

	slog.Debug("should NOT appear", "k", "v")
	if buf.Len() != 0 {
		t.Errorf("expected no output at the default level, got: %q", buf.String())
	}

	slog.Info("should appear", "k", "v")
	if !strings.Contains(buf.String(), "INFO should appear") {
		t.Errorf("expected an INFO line in output, got: %q", buf.String())
	}
}

// TestApplyLogLevel_InvalidLevel_FallsBackToDefaultWithWarning pins the
// decision (task instructions: "決めて pin する") for a log.level string
// ApplyLogLevel does not recognize: this path should be unreachable in
// practice (Config.UnmarshalYAML already rejects it at config-load time —
// see internal/config.TestLoadFromPath_LogLevel_InvalidRejected), but
// defensively, ApplyLogLevel warns and leaves the level at its default
// (info) rather than panicking or silently enabling debug output.
func TestApplyLogLevel_InvalidLevel_FallsBackToDefaultWithWarning(t *testing.T) {
	buf := captureStdLog(t)
	ApplyLogLevel("verbose") // not one of config.LogLevelNames

	slog.Debug("should NOT appear (invalid level fell back to default)", "k", "v")
	if strings.Contains(buf.String(), "should NOT appear") {
		t.Errorf("invalid log.level unexpectedly enabled debug output: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("expected a WARN line about the invalid level, got: %q", buf.String())
	}
}

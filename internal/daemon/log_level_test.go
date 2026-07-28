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
// the duration of the test, restoring the previous writer, slog's
// log-package bridge level (reset to its zero-value default, LevelInfo),
// AND slog's default Logger on cleanup. The default-Logger restore matters
// even though nothing in this package calls slog.SetDefault today: `go
// test` runs every test in a package binary sequentially, and slog.Default
// is process-wide — internal/config, internal/orchestrator, and
// internal/server's own test suites each install a captured
// slog.NewTextHandler default for the duration of a test (see e.g.
// internal/config/config_test.go's captureSlog); the day any of that ever
// lands in this package too, an unrestored default here would leak into
// whichever test in this file's binary runs next, and that test's own
// assertions would fail for a reason having nothing to do with its own
// logic.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevDefault := slog.Default()
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags)
	slog.SetLogLoggerLevel(slog.LevelInfo)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		slog.SetLogLoggerLevel(slog.LevelInfo)
		slog.SetDefault(prevDefault)
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

// TestApplyLogLevel_InvalidLevel_KeepsCurrentLevelWithWarning pins the
// decision (task instructions: "決めて pin する") for a log.level string
// ApplyLogLevel does not recognize: this path should be unreachable in
// practice (Config.UnmarshalYAML already rejects it at config-load time —
// see internal/config.TestLoadFromPath_LogLevel_InvalidRejected), but
// defensively, ApplyLogLevel warns and returns without changing the level
// AT ALL — it does not reset to any default, it just leaves whatever the
// level already was. Deliberately verified from a non-default starting
// level (debug, set by a prior valid call), not from captureStdLog's own
// reset-to-info baseline: asserting only "debug stayed off" after an
// invalid call starting from info would pass even if ApplyLogLevel reset to
// info on every invalid input, which is a materially different (and
// wrong-named) behavior from "leaves the current level untouched".
func TestApplyLogLevel_InvalidLevel_KeepsCurrentLevelWithWarning(t *testing.T) {
	buf := captureStdLog(t)
	ApplyLogLevel("debug") // establish a non-default current level first
	buf.Reset()

	ApplyLogLevel("verbose") // not one of config.LogLevelNames; must not touch the level set above

	slog.Debug("should still appear (invalid level must not reset the level)", "k", "v")
	if !strings.Contains(buf.String(), "DEBUG should still appear") {
		t.Errorf("expected the prior debug level to survive an invalid ApplyLogLevel call, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("expected a WARN line about the invalid level, got: %q", buf.String())
	}
}

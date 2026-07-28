package config

import (
	"log/slog"
	"testing"
)

// TestParseLogLevel_Valid pins the log.level config.yaml string -> slog.Level
// mapping ParseLogLevel exposes: every entry in LogLevelNames must round-trip
// to its expected slog.Level constant. LogLevelNames doubles as schema.go's
// "log.level" KindEnum.EnumValues, so this also indirectly pins that the
// enum's accepted strings are exactly the ones ParseLogLevel understands.
func TestParseLogLevel_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for _, tc := range cases {
		got, err := ParseLogLevel(tc.in)
		if err != nil {
			t.Errorf("ParseLogLevel(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseLogLevel_Invalid pins the decision (task instructions: "決めて
// pin する") that an unrecognized log.level string is a hard error, not a
// silent fallback — mirroring how gateway.forges.*.forge already rejects an
// unrecognized forge value (config.go's resolveForgeConfig) rather than
// picking a default.
func TestParseLogLevel_Invalid(t *testing.T) {
	for _, in := range []string{"", "DEBUG", "trace", "verbose", "warning", " debug"} {
		if _, err := ParseLogLevel(in); err == nil {
			t.Errorf("ParseLogLevel(%q) expected error, got nil", in)
		}
	}
}

// TestLogLevelNames_MatchesParseLogLevel pins that every name ParseLogLevel
// accepts is present in LogLevelNames and vice versa, so schema.go's
// EnumValues (which is LogLevelNames) can never silently drift out of sync
// with what ParseLogLevel itself accepts.
func TestLogLevelNames_MatchesParseLogLevel(t *testing.T) {
	want := []string{"debug", "info", "warn", "error"}
	if len(LogLevelNames) != len(want) {
		t.Fatalf("LogLevelNames = %v, want %v", LogLevelNames, want)
	}
	for i, w := range want {
		if LogLevelNames[i] != w {
			t.Errorf("LogLevelNames[%d] = %q, want %q", i, LogLevelNames[i], w)
		}
	}
	for _, name := range LogLevelNames {
		if _, err := ParseLogLevel(name); err != nil {
			t.Errorf("ParseLogLevel(%q) (from LogLevelNames) unexpected error: %v", name, err)
		}
	}
}

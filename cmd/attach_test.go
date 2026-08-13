package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestShowLogPagerWithCmds_Fallback verifies that when no pager command is
// available the output is written to stdout and a "press any key" prompt is shown.
func TestShowLogPagerWithCmds_Fallback(t *testing.T) {
	const testOutput = "hello pager\nline2\nline3"
	var stdout bytes.Buffer
	stdin := strings.NewReader("x") // simulate a keypress

	err := showLogPagerWithCmds(testOutput, &stdout, stdin, nil)
	if err != nil {
		t.Fatalf("showLogPagerWithCmds: unexpected error: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, testOutput) {
		t.Errorf("fallback: expected output %q in stdout, got %q", testOutput, got)
	}
	if !strings.Contains(got, "press any key") {
		t.Errorf("fallback: expected 'press any key' prompt in stdout, got %q", got)
	}
}

// TestShowLogPagerWithCmds_FallbackEmptyOutput verifies that even with empty
// output the "press any key" prompt is shown.
func TestShowLogPagerWithCmds_FallbackEmptyOutput(t *testing.T) {
	var stdout bytes.Buffer
	stdin := strings.NewReader("x")

	err := showLogPagerWithCmds("", &stdout, stdin, nil)
	if err != nil {
		t.Fatalf("showLogPagerWithCmds empty: unexpected error: %v", err)
	}

	if !strings.Contains(stdout.String(), "press any key") {
		t.Errorf("fallback empty output: expected 'press any key' prompt, got %q", stdout.String())
	}
}

// TestShowLogPagerWithCmds_SkipsUnknownCommand verifies that a non-existent
// pager binary is silently skipped and the fallback is used.
func TestShowLogPagerWithCmds_SkipsUnknownCommand(t *testing.T) {
	const testOutput = "skipped pager output"
	var stdout bytes.Buffer
	stdin := strings.NewReader("x")

	err := showLogPagerWithCmds(testOutput, &stdout, stdin, [][]string{
		{"__nonexistent_pager_binary__"},
	})
	if err != nil {
		t.Fatalf("showLogPagerWithCmds skip: unexpected error: %v", err)
	}

	if !strings.Contains(stdout.String(), testOutput) {
		t.Errorf("after skipping unknown pager: expected output in stdout, got %q", stdout.String())
	}
}

// TestPagerCommands_ContainsLessAndMore verifies default pager candidates include
// less and more.
func TestPagerCommands_ContainsLessAndMore(t *testing.T) {
	cmds := pagerCommands()
	hasLess, hasMore := false, false
	for _, c := range cmds {
		if len(c) > 0 && c[0] == "less" {
			hasLess = true
		}
		if len(c) > 0 && c[0] == "more" {
			hasMore = true
		}
	}
	if !hasLess {
		t.Error("pagerCommands: expected 'less' in candidates")
	}
	if !hasMore {
		t.Error("pagerCommands: expected 'more' in candidates")
	}
}

// TestWriteTerminalEpilogue_ResetsMouseReporting pins the fix for the
// Windows-sleep symptom: a job whose harness turned on mouse reporting
// (Claude Code's TUI sends DECSET 1000/1002/1003/1006 on startup) normally
// turns it back off on exit, and those bytes reach the local terminal
// through the attach stream. When the wire dies instead — a laptop
// suspending mid-session — the "off" half never arrives and the local
// terminal is left forwarding drags to nobody, so the user cannot select
// the very error message that explains the drop. attachLive therefore
// writes the "off" half itself on the way out.
func TestWriteTerminalEpilogue_ResetsMouseReporting(t *testing.T) {
	var buf bytes.Buffer
	writeTerminalEpilogue(&buf)

	got := buf.String()
	for _, want := range []string{
		"\x1b[?1000l", // X10/normal mouse tracking
		"\x1b[?1002l", // button-event tracking
		"\x1b[?1003l", // any-event tracking
		"\x1b[?1006l", // SGR extended coordinates
		"\x1b[?1015l", // urxvt extended coordinates
		"\x1b[?2004l", // bracketed paste
		"\x1b[?25h",   // cursor visible
	} {
		if !strings.Contains(got, want) {
			t.Errorf("epilogue: missing %q, got %q", want, got)
		}
	}
}

// TestWriteTerminalEpilogue_KeepsAlternateScreen guards the one reset
// deliberately left out. Leaving the alternate screen (DECRST 1049) would
// swap the terminal back to the primary buffer and take the drop notice
// ("connection lost and could not be restored: ...") off screen with it —
// the exact text the user is trying to copy. Mouse reporting is what breaks
// selection; the alternate screen does not, so it stays.
func TestWriteTerminalEpilogue_KeepsAlternateScreen(t *testing.T) {
	var buf bytes.Buffer
	writeTerminalEpilogue(&buf)

	if strings.Contains(buf.String(), "\x1b[?1049l") {
		t.Errorf("epilogue: must not leave the alternate screen, got %q", buf.String())
	}
}

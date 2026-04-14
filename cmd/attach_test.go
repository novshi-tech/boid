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

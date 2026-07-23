package main

import (
	"testing"
)

func TestShouldRunBoidBuiltinShim(t *testing.T) {
	t.Setenv("BOID_BUILTIN_SHIM", "")
	if shouldRunBoidBuiltinShim("boid", []string{"boid", "task"}) {
		t.Fatal("expected boid shim to be disabled without BOID_BUILTIN_SHIM")
	}
	if shouldRunBoidBuiltinShim("git", []string{"git", "status"}) {
		t.Fatal("expected non-boid command to stay on normal path")
	}

	t.Setenv("BOID_BUILTIN_SHIM", "1")
	if !shouldRunBoidBuiltinShim("boid", []string{"boid", "task"}) {
		t.Fatal("expected boid shim to be enabled when BOID_BUILTIN_SHIM is set")
	}
	if shouldRunBoidBuiltinShim("git", []string{"git", "status"}) {
		t.Fatal("expected non-boid command to ignore BOID_BUILTIN_SHIM")
	}
}

// TestShouldRunBoidBuiltinShim_ReservedRunnerSubcommandsAlwaysBypass is the
// PR9 regression guard (docs/plans/phase6-cutover-followups.md's
// e2e-container debugging trail): the container backend applies spec.Env
// (including BOID_BUILTIN_SHIM=1, whenever the job declares
// capabilities.docker) to the job container's OWN PID1 from container
// creation — i.e. to "boid runner-container" itself, not just to a nested
// "boid task update" a hook script might later run. Every "boid
// runner-*" internal entrypoint must always reach the real cmd.Execute()
// dispatch regardless of BOID_BUILTIN_SHIM, or the container backend's
// own entrypoint process misroutes into "unsupported boid subcommand"
// and the job never runs at all.
func TestShouldRunBoidBuiltinShim_ReservedRunnerSubcommandsAlwaysBypass(t *testing.T) {
	t.Setenv("BOID_BUILTIN_SHIM", "1")
	for _, sub := range []string{"runner-outer", "runner-inner", "runner-inner-child", "runner-container"} {
		argv := []string{"boid", sub, "--spec", "/run/boid/spec.json", "--state", "/run/boid/state.json"}
		if shouldRunBoidBuiltinShim("boid", argv) {
			t.Errorf("shouldRunBoidBuiltinShim(%q) = true, want false (reserved internal entrypoint must bypass the builtin shim even with BOID_BUILTIN_SHIM=1)", sub)
		}
	}
	// A non-reserved subcommand (e.g. a nested "boid task update" call from
	// within a hook script) must still route through the shim.
	if !shouldRunBoidBuiltinShim("boid", []string{"boid", "task", "update"}) {
		t.Error("shouldRunBoidBuiltinShim(\"task\") = false, want true (non-reserved subcommands must still use the builtin shim under BOID_BUILTIN_SHIM=1)")
	}
}

func TestIsReservedRunnerSubcommand(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"boid"}, false},
		{[]string{"boid", "task"}, false},
		{[]string{"boid", "runner-outer"}, true},
		{[]string{"boid", "runner-inner"}, true},
		{[]string{"boid", "runner-inner-child"}, true},
		{[]string{"boid", "runner-container"}, true},
		{[]string{}, false},
	}
	for _, c := range cases {
		if got := isReservedRunnerSubcommand(c.argv); got != c.want {
			t.Errorf("isReservedRunnerSubcommand(%v) = %v, want %v", c.argv, got, c.want)
		}
	}
}

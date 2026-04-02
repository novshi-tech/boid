package main

import (
	"testing"
)

func TestShouldRunBoidBuiltinShim(t *testing.T) {
	t.Setenv("BOID_BUILTIN_SHIM", "")
	if shouldRunBoidBuiltinShim("boid") {
		t.Fatal("expected boid shim to be disabled without BOID_BUILTIN_SHIM")
	}
	if shouldRunBoidBuiltinShim("git") {
		t.Fatal("expected non-boid command to stay on normal path")
	}

	t.Setenv("BOID_BUILTIN_SHIM", "1")
	if !shouldRunBoidBuiltinShim("boid") {
		t.Fatal("expected boid shim to be enabled when BOID_BUILTIN_SHIM is set")
	}
	if shouldRunBoidBuiltinShim("git") {
		t.Fatal("expected non-boid command to ignore BOID_BUILTIN_SHIM")
	}
}

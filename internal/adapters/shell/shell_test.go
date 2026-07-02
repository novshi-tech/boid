package shell_test

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/adapters/shell"
)

// TestUsage_ReturnsZero pins the shell adapter's non-billable contract: shell
// jobs (hooks / exec) carry no token accounting, so Usage must always return
// the zero value and a nil error. A regression that started reporting non-zero
// usage would leak bogus metrics into Phase 4 accounting.
func TestUsage_ReturnsZero(t *testing.T) {
	u, err := shell.New().Usage(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.Model != "" || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("Usage = %+v, want zero", u)
	}
}

// TestBindings_ReturnsNil pins the "no harness-specific binds" contract: shell
// inherits only the base mount set the dispatcher applies to every sandbox. If
// this ever returned a non-nil set the dispatcher would add unexpected mounts
// to plain exec/hook jobs.
func TestBindings_ReturnsNil(t *testing.T) {
	if got := shell.New().Bindings("/home/test"); got != nil {
		t.Errorf("Bindings = %v, want nil", got)
	}
}

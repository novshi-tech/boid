package client

import (
	"context"
	"testing"
)

func TestFromContext_NilContext_FallsBackToUnixDefault(t *testing.T) {
	c := FromContext(nil)
	if c == nil {
		t.Fatal("FromContext(nil) returned nil")
	}
	if !c.IsUnix() {
		t.Error("expected the fallback client to be unix-scheme")
	}
}

func TestFromContext_NoClientInjected_FallsBackToUnixDefault(t *testing.T) {
	c := FromContext(context.Background())
	if c == nil {
		t.Fatal("FromContext returned nil")
	}
	if !c.IsUnix() {
		t.Error("expected the fallback client to be unix-scheme")
	}
}

func TestWithClient_FromContext_RoundTrips(t *testing.T) {
	want := NewUnixClient("/tmp/does-not-matter.sock")
	ctx := WithClient(context.Background(), want)
	got := FromContext(ctx)
	if got != want {
		t.Errorf("FromContext returned a different client than was injected")
	}
}

func TestWithClient_NilParentContext_DoesNotPanic(t *testing.T) {
	want := NewUnixClient("/tmp/does-not-matter.sock")
	ctx := WithClient(nil, want) //nolint:staticcheck // exercising the documented nil-parent fallback
	got := FromContext(ctx)
	if got != want {
		t.Errorf("FromContext returned a different client than was injected")
	}
}

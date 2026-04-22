package qrterm_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/qrterm"
)

func TestEncode(t *testing.T) {
	t.Run("unicode", func(t *testing.T) {
		s, err := qrterm.Encode("https://example.com", false)
		if err != nil {
			t.Fatal(err)
		}
		if s == "" {
			t.Error("expected non-empty string")
		}
	})

	t.Run("ascii", func(t *testing.T) {
		s, err := qrterm.Encode("https://example.com", true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s, "##") {
			t.Errorf("expected ## in ascii output, got: %q", s[:min(len(s), 100)])
		}
	})
}

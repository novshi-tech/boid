package orchestrator

import (
	"strings"
	"testing"
)

func TestValidWorkspaceSlug(t *testing.T) {
	t.Parallel()

	valid := []string{
		"node",
		"go-dev",
		"a",
		"0-1-2",
		"abc123",
		"my-workspace",
		strings.Repeat("a", 64), // exactly 64 chars — boundary
	}

	for _, slug := range valid {
		slug := slug
		displaySlug := slug
		if len(displaySlug) > 20 {
			displaySlug = displaySlug[:20]
		}
		t.Run("valid/"+displaySlug, func(t *testing.T) {
			t.Parallel()
			if err := ValidWorkspaceSlug(slug); err != nil {
				t.Errorf("ValidWorkspaceSlug(%q) = %v, want nil", slug, err)
			}
		})
	}

	type invalidCase struct {
		slug string
		desc string
	}
	invalid := []invalidCase{
		{"", "empty"},
		{"/", "slash"},
		{"..", "dotdot"},
		{"Node", "uppercase"},
		{"my_kit", "underscore"},
		{"with space", "space"},
		{strings.Repeat("a", 65), "65 chars"},
		{"日本語", "japanese"},
		{"foo/bar", "contains slash"},
		{"foo bar", "contains space"},
		{"FOO", "all uppercase"},
		{"foo.bar", "dot"},
	}

	for _, tc := range invalid {
		tc := tc
		t.Run("invalid/"+tc.desc, func(t *testing.T) {
			t.Parallel()
			if err := ValidWorkspaceSlug(tc.slug); err == nil {
				t.Errorf("ValidWorkspaceSlug(%q) = nil, want error", tc.slug)
			}
		})
	}
}


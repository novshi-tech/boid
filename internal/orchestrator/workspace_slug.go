package orchestrator

import "fmt"

// DefaultWorkspaceSlug is the reserved slug for the implicit "default"
// workspace that every project belongs to when no explicit assignment is
// chosen. It is auto-created at daemon startup, used as the fallback target
// for `project add` / `project migrate` when --workspace is omitted, and
// guarded against deletion by WorkspaceStore.Remove.
const DefaultWorkspaceSlug = "default"

// ValidWorkspaceSlug checks that s is a valid workspace slug.
// A valid slug consists of lowercase ASCII letters, digits, and hyphens,
// is between 1 and 64 characters long, and contains no other characters.
func ValidWorkspaceSlug(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("workspace slug %q is empty", s)
	}
	if len(s) > 64 {
		return fmt.Errorf("workspace slug %q exceeds 64 chars", s)
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return fmt.Errorf("workspace slug %q contains invalid character %q", s, string(r))
		}
	}
	return nil
}

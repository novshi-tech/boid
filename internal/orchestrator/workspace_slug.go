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
//
// The character rule is the whole rule: there is deliberately no
// reserved-word list. "export" and "apply" are static path segments on the
// workspace router at the same depth as its "/{slug}" wildcard, so a
// workspace named either one is shadowed for the methods those static
// routes register (GET /api/workspaces/export reaches the envelope export,
// never Show). That collision is known and accepted rather than validated
// away — rejecting the names here would turn any workspace already created
// under one of them into a workspace the daemon refuses to load, which is a
// worse failure than the partial shadowing it prevents. See
// api.WorkspaceHandler.Routes for the full decision record and the test
// that pins it.
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

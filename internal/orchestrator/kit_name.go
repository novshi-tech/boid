package orchestrator

import "fmt"

// ValidKitName checks that s is a valid kit name.
// A valid name consists of lowercase ASCII letters, digits, and hyphens,
// is between 1 and 64 characters long, and contains no other characters.
func ValidKitName(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("kit name %q is empty", s)
	}
	if len(s) > 64 {
		return fmt.Errorf("kit name %q exceeds 64 chars", s)
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return fmt.Errorf("kit name %q contains invalid character %q", s, string(r))
		}
	}
	return nil
}

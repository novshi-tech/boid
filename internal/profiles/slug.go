package profiles

import (
	"fmt"
	"regexp"
)

// slugPattern is decision 5 (docs/plans/cli-remote-connection.md): a
// profile name must start with a lowercase letter or digit and contain only
// lowercase letters, digits, underscore, and hyphen thereafter. This
// excludes path separators, "..", and any other character that would let a
// profile name picked from --profile/BOID_PROFILE/default_profile double as
// a path-traversal payload when it is later used to build
// ~/.config/boid/tokens/<profile>.json (token.go's TokenPath).
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidateSlug reports whether name is a valid profile name. Called
// wherever a profile name is actually about to be used to look something
// up (Resolve) or written to disk (a future PR2's `boid login --profile`),
// not by Config's own parsing (config.go's Config/UnmarshalYAML accept
// arbitrary map keys so a hand-edited config.yaml with a bad key still
// parses — Resolve is where an invalid name becomes a clear, contextual
// error instead of a low-level regex complaint).
func ValidateSlug(name string) error {
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: must match %s (lowercase letters, digits, underscore, hyphen; cannot start with underscore or hyphen)", name, slugPattern.String())
	}
	return nil
}

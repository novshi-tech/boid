package dockerproxy

import (
	"fmt"
	"os"
	"strings"
)

// ResolveUpstream finds the upstream Docker socket path using this precedence:
//  1. explicit — non-empty value from config (returned as-is, no existence check)
//  2. DOCKER_HOST env var — must be unix:// prefix; TCP returns an error
//  3. $XDG_RUNTIME_DIR/docker.sock — rootless
//  4. /run/user/<uid>/docker.sock — rootless fallback
//  5. /var/run/docker.sock — rootful
func ResolveUpstream(explicit string) (string, error) {
	var candidates []string
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, xdg+"/docker.sock")
	}
	uid := os.Getuid()
	candidates = append(candidates, fmt.Sprintf("/run/user/%d/docker.sock", uid))
	candidates = append(candidates, "/var/run/docker.sock")
	return resolveUpstream(explicit, os.Getenv("DOCKER_HOST"), candidates)
}

// resolveUpstream is the testable core; callers supply dockerHost and candidates.
func resolveUpstream(explicit, dockerHost string, candidates []string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	if dockerHost != "" {
		if strings.HasPrefix(dockerHost, "unix://") {
			path := strings.TrimPrefix(dockerHost, "unix://")
			if path == "" {
				return "", fmt.Errorf("DOCKER_HOST unix:// has empty path")
			}
			return path, nil
		}
		return "", fmt.Errorf("DOCKER_HOST %q is not a unix:// path; TCP upstream is not supported", dockerHost)
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("no Docker socket found; tried: %v", candidates)
}

package dockerproxy

import (
	"fmt"
	"os"
	"strings"
)

// ResolveUpstream finds the upstream Docker socket path using this precedence:
//  1. explicit — non-empty value from config (returned as-is, no existence check)
//  2. DOCKER_HOST env var — must be unix:// prefix; TCP returns an error
//  3. $XDG_RUNTIME_DIR/docker.sock — rootless docker
//  4. $XDG_RUNTIME_DIR/podman/podman.sock — rootless podman
//  5. /run/user/<uid>/docker.sock — rootless docker fallback
//  6. /run/user/<uid>/podman/podman.sock — rootless podman fallback
//  7. /var/run/docker.sock — rootful docker
//  8. /run/podman/podman.sock — rootful podman
//
// docker candidates always rank above podman within the same scope so that
// hosts running both daemons keep their existing behavior; podman is added
// as a fallback for the common case where only podman is installed.
func ResolveUpstream(explicit string) (string, error) {
	var candidates []string
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates,
			xdg+"/docker.sock",
			xdg+"/podman/podman.sock",
		)
	}
	uid := os.Getuid()
	candidates = append(candidates,
		fmt.Sprintf("/run/user/%d/docker.sock", uid),
		fmt.Sprintf("/run/user/%d/podman/podman.sock", uid),
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
	)
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

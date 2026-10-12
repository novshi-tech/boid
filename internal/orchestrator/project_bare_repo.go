package orchestrator

import (
	"fmt"
	"strings"
)

const URLDerivedProjectIDPrefix = "url-"

func IsURLDerivedProjectID(id string) bool {
	return strings.HasPrefix(id, URLDerivedProjectIDPrefix)
}

func ValidProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name is empty")
	}
	if len(name) > 128 {
		return fmt.Errorf("project name %q exceeds 128 chars", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("project name %q must not contain path separators", name)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("project name %q must not start with '.'", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("project name %q must not contain '..'", name)
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-') {
			return fmt.Errorf("project name %q contains invalid character %q", name, string(r))
		}
	}
	return nil
}

func DeriveProjectNameFromURL(rawURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(rawURL), "/")
	if trimmed == "" {
		return "", fmt.Errorf("git URL is empty")
	}
	trimmed = strings.TrimSuffix(trimmed, ".git")
	idx := strings.LastIndexAny(trimmed, "/:")
	if idx == -1 || idx == len(trimmed)-1 {
		return "", fmt.Errorf("cannot derive a project name from git URL %q", rawURL)
	}
	name := trimmed[idx+1:]
	if name == "" {
		return "", fmt.Errorf("cannot derive a project name from git URL %q", rawURL)
	}
	return name, nil
}

package orchestrator

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const URLDerivedProjectIDPrefix = "url-"

func IsURLDerivedProjectID(id string) bool {
	return strings.HasPrefix(id, URLDerivedProjectIDPrefix)
}

func IsBareRepoDir(dir string) bool {
	headInfo, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || headInfo.IsDir() {
		return false
	}
	objInfo, err := os.Stat(filepath.Join(dir, "objects"))
	if err != nil || !objInfo.IsDir() {
		return false
	}
	return true
}

// DefaultBranch returns a bare repo's default branch — its HEAD symbolic
// ref, short form (`git symbolic-ref --short HEAD`).
func DefaultBranch(bareRepoPath string) (string, error) {
	cmd := exec.Command("git", "-C", bareRepoPath, "symbolic-ref", "--short", "HEAD")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%s: git symbolic-ref --short HEAD: %w: %s", bareRepoPath, err, msg)
		}
		return "", fmt.Errorf("%s: git symbolic-ref --short HEAD: %w", bareRepoPath, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func BareRepoPath(dataDir, workspaceSlug, projectName string) string {
	return filepath.Join(dataDir, "repos", workspaceSlug, projectName+".git")
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

func SafeBareRepoPath(dataDir, workspaceSlug, projectName string) (string, error) {
	if err := ValidProjectName(projectName); err != nil {
		return "", err
	}
	root := ReposRoot(dataDir)
	path := BareRepoPath(dataDir, workspaceSlug, projectName)
	underRoot, err := PathIsUnderResolved(root, path)
	if err != nil {
		return "", fmt.Errorf("verify project name %q does not escape the workspace's repo directory: %w", projectName, err)
	}
	if !underRoot {
		return "", fmt.Errorf("project name %q resolves outside the workspace's repo directory", projectName)
	}
	return path, nil
}

func ReposRoot(dataDir string) string {
	return filepath.Join(dataDir, "repos")
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

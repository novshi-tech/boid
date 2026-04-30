package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ExpandBaseBranch expands ${current_branch} (or $current_branch) in value
// by reading the HEAD branch of the git repo at workDir.
// Unknown variables are left as-is for backward compatibility.
// Returns an error if the git command fails or the repo is in detached HEAD state.
func ExpandBaseBranch(value, workDir string) (string, error) {
	if !strings.Contains(value, "current_branch") {
		return value, nil
	}

	var (
		branch    string
		expandErr error
	)
	result := os.Expand(value, func(name string) string {
		if name != "current_branch" {
			return "${" + name + "}"
		}
		if expandErr != nil {
			return ""
		}
		if branch == "" {
			branch, expandErr = gitCurrentBranch(workDir)
		}
		if expandErr != nil {
			return ""
		}
		return branch
	})
	if expandErr != nil {
		return "", expandErr
	}
	return result, nil
}

func gitCurrentBranch(workDir string) (string, error) {
	cmd := exec.Command("git", "-C", workDir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse failed in %s: %w", workDir, err)
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" {
		return "", fmt.Errorf("${current_branch} cannot be expanded: project is in detached HEAD state")
	}
	return b, nil
}

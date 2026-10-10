package adapters

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PrepareWorkspace checks out the primary repository before starting a sandbox command.
func PrepareWorkspace(ctx context.Context, rc RunContext) (RunContext, error) {
	if rc.Env["BOID_PRIMARY_REPO"] == "" {
		return rc, nil
	}
	cmd := exec.CommandContext(ctx, "boid", "checkout")
	cmd.Stderr = rc.Stderr
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	for key, value := range rc.Env {
		env[key] = value
	}
	delete(env, "PWD")
	delete(env, "OLDPWD")
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.Output()
	if err != nil {
		return rc, fmt.Errorf("startup checkout: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" || strings.ContainsAny(dir, "\r\n") {
		return rc, fmt.Errorf("startup checkout: expected one working-tree path")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return rc, fmt.Errorf("startup checkout directory: %w", err)
	}
	if !info.IsDir() {
		return rc, fmt.Errorf("startup checkout path is not a directory")
	}
	rc.Workspace = dir
	if rc.Stderr != nil {
		fmt.Fprintf(rc.Stderr, "[boid] checkout cwd=%s base_branch=%s fork_point=%s (branch selection belongs to the agent)\n", dir, rc.Env["BOID_BASE_BRANCH"], rc.Env["BOID_FORK_POINT"])
	}
	ReportWorkspace(rc, "startup")
	return rc, nil
}

// ReportWorkspace records the current branch in the job transcript.
func ReportWorkspace(rc RunContext, phase string) {
	if rc.Env["BOID_PRIMARY_REPO"] == "" || rc.Stderr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", rc.Workspace, "symbolic-ref", "--quiet", "--short", "HEAD")
	out, err := cmd.Output()
	branch := strings.TrimSpace(string(out))
	if err != nil {
		branch = "unavailable (detached HEAD or unreadable repository)"
	}
	fmt.Fprintf(rc.Stderr, "[boid] git workspace phase=%s cwd=%s branch=%s base_branch=%s\n", phase, rc.Workspace, branch, rc.Env["BOID_BASE_BRANCH"])
}

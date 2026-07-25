package orchestrator

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file implements the git-URL project model (docs/plans/
// volume-only-daemon.md §論点a "project モデル transition (dir → git URL)"
// and §論点b "daemon 管理 bare repository"): reading a registered project's
// project.yaml out of a daemon-managed bare git repository instead of a
// host filesystem checkout. Everything here is pure local git plumbing (no
// network, no credentials) — cloning/fetching a bare repo (which DOES need
// forge credentials) lives in internal/dispatcher (see bare_repo.go there),
// which already imports internal/gitgateway; this package must stay
// dependency-free of both to avoid an import cycle (dispatcher imports
// orchestrator for JobSpec/ProjectMeta/etc.).

// IsBareRepoDir reports whether dir looks like a daemon-managed bare git
// repository (docs/plans/volume-only-daemon.md §論点b layout:
// "<data_dir>/repos/<workspace-slug>/<project-name>.git/") rather than a
// filesystem project checkout (which has its .git as a *subdirectory*, not
// visible at this level, and a .boid/project.yaml directly on disk).
// ProjectStore.LoadAll uses this to decide whether a registered project's
// WorkDir should be loaded via ReadProjectMetaFromBareRepo (this file) or
// ReadProjectMetaWithKits (spec_loader.go, the pre-existing filesystem
// path) — both project models coexist post-PR-2a (see that migration
// path's own doc comment in project_store.go).
//
// Detection: a bare repo has HEAD (a file, not a directory) and objects/ (a
// directory) directly under dir. This is deliberately cheap (two os.Stat
// calls, no git subprocess) since LoadAll runs it once per registered
// project on every daemon startup/reload.
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

// ReadProjectMetaFromBareRepo reads and validates .boid/project.yaml from a
// daemon-managed bare git repository's HEAD (docs/plans/volume-only-daemon.md
// §論点a: "project.yaml は bare repo の HEAD (default branch) から `git show
// HEAD:.boid/project.yaml` で読む"). Shares the exact same validation
// pipeline as the filesystem-based ReadProjectMeta via parseProjectMetaBytes
// (spec_loader.go).
//
// Unlike ReadProjectMeta, there is no project directory on disk to resolve
// a relative host_commands.path against (resolveProjectHostCommandPaths is
// filesystem-only) — a host_commands entry with a non-absolute Path is
// rejected here instead of silently resolved against nothing.
func ReadProjectMetaFromBareRepo(bareRepoPath string) (*ProjectMeta, error) {
	data, err := gitShowHEAD(bareRepoPath, ".boid/project.yaml")
	if err != nil {
		return nil, err
	}

	meta, err := parseProjectMetaBytes(bareRepoPath, data)
	if err != nil {
		return nil, err
	}

	for name, spec := range meta.HostCommands {
		if spec.Path != "" && !filepath.IsAbs(spec.Path) {
			return nil, fmt.Errorf("project.yaml: host_commands.%s.path %q must be an absolute path (relative host_commands paths are not supported for git-URL registered projects, which have no on-disk project directory to resolve them against)", name, spec.Path)
		}
	}

	return meta, nil
}

// gitShowHEAD runs `git show HEAD:<relPath>` inside bareRepoPath and returns
// its stdout. Failure covers every reason the blob might not be readable —
// the bare repo missing/corrupt, HEAD unresolvable (no commits), or relPath
// simply absent from the tree — all of which docs/plans/volume-only-daemon.md
// §論点a's auto-prune-retirement invariant treats identically (mark the
// project degraded, never hard-delete its DB row), so callers do not need a
// finer-grained classification here.
func gitShowHEAD(bareRepoPath, relPath string) ([]byte, error) {
	cmd := exec.Command("git", "-C", bareRepoPath, "show", "HEAD:"+relPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%s: git show HEAD:%s: %w: %s", bareRepoPath, relPath, err, msg)
		}
		return nil, fmt.Errorf("%s: git show HEAD:%s: %w", bareRepoPath, relPath, err)
	}
	return stdout.Bytes(), nil
}

// DefaultBranch returns a bare repo's default branch — its HEAD symbolic
// ref, short form (docs/plans/volume-only-daemon.md §論点a: "Default branch
// determined by bare repo's HEAD symbolic ref (`git symbolic-ref HEAD`)").
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

// BareRepoPath returns the daemon-managed bare repository path for a
// project named projectName registered into workspaceSlug (docs/plans/
// volume-only-daemon.md §論点b layout: "<data_dir>/repos/<workspace-slug>/
// <project-name>.git/"). dataDir is the daemon's data-home
// (~/.local/share/boid equivalent — container-side
// /home/boid/.local/share/boid in the eventual volume-only deploy).
func BareRepoPath(dataDir, workspaceSlug, projectName string) string {
	return filepath.Join(dataDir, "repos", workspaceSlug, projectName+".git")
}

// ReposRoot returns the daemon's bare-repo storage root
// (<dataDir>/repos) — the prefix every BareRepoPath result shares, used by
// ProjectStore.SetReposRoot to distinguish a legacy (pre-cutover,
// host-filesystem) project registration from a git-URL one when neither
// loader can read its project.yaml (see LoadAll's migration-path handling).
func ReposRoot(dataDir string) string {
	return filepath.Join(dataDir, "repos")
}

// DeriveProjectNameFromURL derives a project name from a git remote URL's
// last path component, stripping a trailing ".git" (docs/plans/
// volume-only-daemon.md §論点a: "project-name は URL から derive"). Handles
// both slash-delimited forms (https://host/owner/repo(.git)?,
// ssh://host/owner/repo(.git)?) and the scp-like form
// (git@host:owner/repo(.git)?) since both end in a '/'-delimited path
// segment.
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

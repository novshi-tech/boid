package orchestrator

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// URLDerivedProjectIDPrefix marks a project id derived from a git URL rather
// than read from a committed project.yaml's `id:` field (docs/plans/
// workspace-default-project.md 決定3/論点g — see internal/api's
// deriveProjectIDFromURL, the only producer of an id with this prefix).
//
// Exported so ProjectStore.LoadAll and ProjectAppService.FetchProject (the
// reload paths) can gate their GitHeadReadFailurePathAbsent fallback on it
// (Codex review, PR5 round 2 Major): that failure kind alone does not
// distinguish "this project was registered without a project.yaml on
// purpose" from "this is an ordinary project.yaml-bearing project whose
// .boid/project.yaml was deleted upstream by mistake" — both read back
// identically from gitShowHEAD. Silently switching the latter over to the
// workspace default on next reload/fetch would hide the deletion and swap
// its task_behaviors out from under it without any degraded signal.
// Gating on this prefix limits the fallback to projects this daemon itself
// registered via the no-project.yaml path; every other project keeps
// degrading exactly as before.
const URLDerivedProjectIDPrefix = "url-"

// IsURLDerivedProjectID reports whether id was derived from a git URL
// (project.yaml-less registration, PR5) rather than read from a committed
// project.yaml's `id:` field.
func IsURLDerivedProjectID(id string) bool {
	return strings.HasPrefix(id, URLDerivedProjectIDPrefix)
}

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
// a relative host_commands.path against — a host_commands entry with a
// non-absolute Path is rejected here instead of silently resolved against
// nothing.
func ReadProjectMetaFromBareRepo(bareRepoPath string) (*ProjectMeta, error) {
	data, err := gitShowHEAD(bareRepoPath, ".boid/project.yaml")
	if err != nil {
		return nil, err
	}

	meta, err := parseProjectMetaBytes(bareRepoPath, true, data)
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
// §論点a's auto-prune-retirement invariant treats identically today (mark
// the project degraded, never hard-delete its DB row).
//
// The returned error is a *GitHeadReadError classifying WHICH of those
// shapes it was (docs/plans/workspace-default-project.md 論点c) — scaffolding
// for a future PR that will let "relPath absent" fall back to a
// workspace-level default instead of failing outright, while every other
// shape keeps failing exactly as it does today. No caller reads
// GitHeadReadError.Kind yet: they all just check err != nil, and Error()
// reproduces this function's pre-existing plain-error message byte for
// byte, so today's behavior is unchanged.
func gitShowHEAD(bareRepoPath, relPath string) ([]byte, error) {
	cmd := exec.Command("git", "-C", bareRepoPath, "show", "HEAD:"+relPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		var plain error
		if msg != "" {
			plain = fmt.Errorf("%s: git show HEAD:%s: %w: %s", bareRepoPath, relPath, err, msg)
		} else {
			plain = fmt.Errorf("%s: git show HEAD:%s: %w", bareRepoPath, relPath, err)
		}
		return nil, &GitHeadReadError{
			BareRepoPath: bareRepoPath,
			RelPath:      relPath,
			Kind:         classifyGitHeadReadFailure(bareRepoPath, relPath),
			err:          plain,
		}
	}
	return stdout.Bytes(), nil
}

// GitHeadReadFailureKind classifies why gitShowHEAD failed to read relPath
// out of a bare repo's HEAD tree (docs/plans/workspace-default-project.md
// 論点c).
type GitHeadReadFailureKind int

const (
	// GitHeadReadFailureOther is the catch-all: HEAD resolves fine (`git
	// rev-parse --verify HEAD` succeeded) but reading relPath still failed
	// for some other reason. Rare in practice — HeadUnresolved and
	// PathAbsent should cover nearly everything gitShowHEAD sees.
	GitHeadReadFailureOther GitHeadReadFailureKind = iota
	// GitHeadReadFailureHeadUnresolved means `git rev-parse --verify HEAD`
	// itself failed: either a bare repo with zero commits yet, or the
	// directory is not a valid git repository at all. The design doc groups
	// both under this one kind because both fail registration identically
	// ("HEAD が解決できない ... repo 自体の異常なので従来通り失敗させる").
	GitHeadReadFailureHeadUnresolved
	// GitHeadReadFailurePathAbsent means HEAD resolves fine but relPath is
	// simply not present in its tree — the "project.yaml was never
	// committed" case a future PR will treat differently from the other
	// two kinds (fall back to a workspace default instead of failing).
	GitHeadReadFailurePathAbsent
)

// String renders the failure kind for logging/diagnostics.
func (k GitHeadReadFailureKind) String() string {
	switch k {
	case GitHeadReadFailureHeadUnresolved:
		return "head-unresolved"
	case GitHeadReadFailurePathAbsent:
		return "path-absent"
	default:
		return "other"
	}
}

// GitHeadReadError is the typed error gitShowHEAD wraps its failures in.
// Callers use errors.As to extract Kind; Error() and Unwrap() preserve
// gitShowHEAD's pre-existing plain-error message and %w chain exactly, so
// every current caller (which only checks err != nil) is unaffected — same
// pattern as ProjectMigrationError (migration_error.go).
type GitHeadReadError struct {
	// BareRepoPath and RelPath are the arguments gitShowHEAD was called
	// with, for callers that want to log/report without re-parsing Error().
	BareRepoPath string
	RelPath      string
	Kind         GitHeadReadFailureKind
	err          error
}

func (e *GitHeadReadError) Error() string { return e.err.Error() }
func (e *GitHeadReadError) Unwrap() error { return e.err }

// classifyGitHeadReadFailure determines why reading relPath out of
// bareRepoPath's HEAD tree failed, using exit codes and stdout only — never
// stderr text, since git's stderr messages are gettext-localized and
// therefore locale/git-version dependent (docs/plans/
// workspace-default-project.md 論点c, fable review m5). Two git
// subprocesses, in order:
//
//  1. `git rev-parse --verify HEAD` — fails exactly when HEAD does not
//     resolve to a commit (empty repo, or the bare repo isn't a valid git
//     repository at all).
//  2. `git ls-tree HEAD -- <relPath>` — once HEAD resolves, this always
//     exits 0 (even for an absent path); empty stdout means relPath is not
//     in the tree, non-empty stdout means it IS present (so whatever made
//     gitShowHEAD's own `git show` fail was something else entirely).
//
// Only called from gitShowHEAD's failure path — the success path never pays
// for these extra subprocesses.
func classifyGitHeadReadFailure(bareRepoPath, relPath string) GitHeadReadFailureKind {
	verify := exec.Command("git", "-C", bareRepoPath, "rev-parse", "--verify", "HEAD")
	if err := verify.Run(); err != nil {
		return GitHeadReadFailureHeadUnresolved
	}

	lsTree := exec.Command("git", "-C", bareRepoPath, "ls-tree", "HEAD", "--", relPath)
	var lsOut bytes.Buffer
	lsTree.Stdout = &lsOut
	if err := lsTree.Run(); err != nil {
		return GitHeadReadFailureOther
	}
	if strings.TrimSpace(lsOut.String()) == "" {
		return GitHeadReadFailurePathAbsent
	}
	return GitHeadReadFailureOther
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
//
// This is a plain filepath.Join with no validation of its own — a
// projectName such as "../../../../tmp/owned" cleans straight through it
// and moves the result outside dataDir/repos/workspaceSlug entirely (PR-2a
// codex round-1 Blocker 2). Callers that accept projectName from an
// external caller (an API request body's --name override, ultimately) MUST
// use SafeBareRepoPath below instead, not this function directly.
func BareRepoPath(dataDir, workspaceSlug, projectName string) string {
	return filepath.Join(dataDir, "repos", workspaceSlug, projectName+".git")
}

// ValidProjectName checks that name is safe to use as the last path segment
// of a daemon-managed bare-repo path (BareRepoPath's projectName argument) —
// PR-2a codex round-1 Blocker 2. Rejects:
//   - empty, or longer than 128 chars;
//   - any path separator ('/' or '\'), which would let projectName escape
//     its intended single path segment entirely (e.g. "sub/dir");
//   - a name that is exactly "." or ".." or starts with '.' (a leading dot
//     is never meaningful for a project name and ".."-prefixed forms are
//     exactly the traversal shape the concrete failure scenario uses:
//     "../../../../tmp/owned" cleans through filepath.Join and moves
//     CreateProjectFromGitURL's clone destination outside
//     <data_dir>/repos/<workspace>, persisting that external path as
//     work_dir and potentially handing it to os.RemoveAll during rollback);
//   - a ".." component anywhere else in the name (defense in depth: even a
//     non-leading "foo..bar" is rejected, since there is no legitimate
//     project name that needs it and it costs nothing to disallow);
//   - any character outside [a-zA-Z0-9._-].
//
// Applies to BOTH an explicit --name override and a URL-derived name
// (DeriveProjectNameFromURL only strips a trailing ".git" and takes the
// last '/'- or ':'-delimited segment of whatever URL string it was given —
// it does not itself guarantee the result is traversal-safe against an
// adversarial URL, so CreateProjectFromGitURL must validate the derived
// name too, not just an explicit override).
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

// SafeBareRepoPath is BareRepoPath with defense-in-depth validation applied
// (PR-2a codex round-1 Blocker 2): projectName must satisfy ValidProjectName
// AND the resulting join must still resolve beneath
// <dataDir>/repos/<workspaceSlug> — the latter check protects against both
// a bug in ValidProjectName's character-level allowlist and any future
// change to BareRepoPath's own Join call that reintroduces a traversal
// (belt-and-suspenders: input validation at the edge, output validation at
// the boundary it is meant to protect). Every caller that accepts
// projectName from outside the daemon process (currently just
// ProjectAppService.CreateProjectFromGitURL's --name / URL-derived name)
// must go through this, not the raw BareRepoPath.
//
// The containment check runs through PathIsUnderResolved against
// ReposRoot(dataDir) — <dataDir>/repos, WITHOUT the workspaceSlug segment —
// not the plain (lexical-only) PathIsUnder against the per-workspace
// directory round-1 used (PR-2a codex round-2 Blocker 2). Two things follow
// from that choice:
//
//   - Symlink defense: <dataDir>/repos/<workspaceSlug> ITSELF being
//     replaced with a symlink to somewhere outside dataDir entirely (codex's
//     concrete scenario) is only detectable by resolving through it and
//     comparing against a boundary that sits ABOVE the symlink — comparing
//     the resolved bareRepoPath against the resolved
//     <dataDir>/repos/<workspaceSlug> itself is circular (path is always
//     lexically a child of root by construction, so resolving both through
//     the SAME symlink trivially keeps that child/parent relationship no
//     matter where the symlink points). ReposRoot(dataDir) is workspace-slug
//     scoping, but is the boundary an attacker would actually need to
//     escape and matches isManagedBareRepoPath's own convention below.
//   - No loss of protection for the original round-1 traversal-name
//     scenario: projectName is already constrained by ValidProjectName to a
//     single clean path segment with no separators and no ".." anywhere —
//     BareRepoPath's Join always places it immediately after workspaceSlug,
//     so a validated projectName can never affect which workspace's
//     directory the result lands in regardless of which boundary this
//     checks against; only an escape past dataDir/repos entirely is
//     actually reachable, and every ".."-based escape used to get there
//     starts by leaving <dataDir>/repos/<workspaceSlug> anyway.
//
// Safe to call even though neither root nor path exists yet at register
// time (the case this function always runs in, BEFORE any clone) — see
// PathIsUnderResolved's own doc comment.
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

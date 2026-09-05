package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"

	"golang.org/x/sys/unix"
)

//go:embed data/boid-web data/boid-orchestrate data/boid-task data/boid-signal data/boid-metaproject
var skillsFS embed.FS

// DeployAll extracts all embedded skill directories under baseDir.
// Each skill is deployed to baseDir/<skill-name>/.
// Files are only written when their content differs from the embedded version.
//
// baseDir is entirely the caller's choice and this package makes no claim
// about where it lives — a general-purpose package naming its caller's
// layout would go stale the moment that layout changes, so the errors below
// quote the path and say nothing else about it.
//
// There is no longer a daemon-side production caller at all: the sandbox skill
// set reaches a job by being baked into the runner image at build time
// (build/container/Dockerfile copies internal/skills/data to
// /opt/boid/skills) and symlinked into the workspace home by the init
// container. What still calls in here is the HOST-skill sibling
// (DeployHostSkills, deploy_host.go) behind `boid install-skills`.
//
// Every write below goes through the symlink-safe, fd-relative helpers in
// safe_deploy.go rather than string-path-based os.MkdirAll/os.CreateTemp/
// os.Rename — see safe_deploy.go's package comment for the threat model this
// guards against. It is retained even though the current default caller's
// baseDir is out of reach of any job: the same helpers also serve
// MkdirAllNoSymlink, whose caller DOES write into the job-owned workspace
// HOME, and nothing about them costs anything on a directory jobs cannot
// reach. A future caller is therefore free to point baseDir at
// attacker-writable storage again without re-deriving the threat model.
func DeployAll(baseDir string) error {
	return deployAllFrom(skillsFS, "data", baseDir)
}

// deployAllFrom is DeployAll generalized over its embed source: sourceFS is
// the embed.FS to read from, root is the top-level directory within it
// (e.g. "data" for skillsFS, "hostdata" for hostSkillsFS — see
// deploy_host.go). Kept unexported: callers outside this package always go
// through a named entry point (DeployAll, DeployHostSkills) that pins its
// own source, so it's not possible to accidentally mix the sandbox and host
// skill sets by passing the wrong (sourceFS, root) pair from outside the
// package.
func deployAllFrom(sourceFS fs.FS, root, baseDir string) error {
	entries, err := fs.ReadDir(sourceFS, root)
	if err != nil {
		return fmt.Errorf("read skills dir: %w", err)
	}

	baseFd, err := openBaseDirSafe(baseDir)
	if err != nil {
		return fmt.Errorf("skills の materialize 先 %q に symlink 混入を検出、または safe path 解決に失敗: %w", baseDir, err)
	}
	defer func() { _ = unix.Close(baseFd) }()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := deploySkill(sourceFS, root, baseFd, e.Name()); err != nil {
			return fmt.Errorf("skills の materialize 先 %q: %w", baseDir, err)
		}
	}
	return nil
}

// EmbeddedSkillNames returns the slugs of the embedded skill directories in
// stable lexical order. dispatcher uses it to compute the claude-side
// ~/.claude/skills/<name> bind targets without hard-coding the list — both
// the mkdir of those targets inside the workspace HOME and the per-skill
// read-only mounts homeMounts declares (internal/dispatcher/
// skills_overlay.go and sandbox_builder.go).
func EmbeddedSkillNames() []string {
	entries, err := fs.ReadDir(skillsFS, "data")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// deploySkill safely deploys the embedded skill directory "data/<name>"
// into the directory named name directly under baseFd, creating that
// directory if it doesn't exist yet and refusing if any path component
// involved turns out to be a symlink (see safe_deploy.go).
func deploySkill(sourceFS fs.FS, root string, baseFd int, name string) error {
	skillFd, err := openOrCreateDirNoSymlink(baseFd, name)
	if err != nil {
		return fmt.Errorf("skill %q: %w", name, err)
	}
	defer func() { _ = unix.Close(skillFd) }()
	return deploySkillDir(sourceFS, skillFd, root+"/"+name)
}

// deploySkillDir mirrors the embedded directory at embedPath into dirFd,
// recursing into subdirectories. It reclaims any stale atomic-write temp
// file left in dirFd before writing anything new — the recovery half of the
// crash-safety contract writeFileSafeAt implements (PR #789 review,
// Should-fix #1): a daemon killed mid-write (SIGKILL, power loss) leaves a
// temp file with no deferred cleanup ever running for it, so the *next*
// deploy pass over the same directory has to sweep it up instead.
func deploySkillDir(sourceFS fs.FS, dirFd int, embedPath string) error {
	if err := cleanupStaleTempFiles(dirFd); err != nil {
		return fmt.Errorf("clean up stale temp files under %q: %w", embedPath, err)
	}

	entries, err := fs.ReadDir(sourceFS, embedPath)
	if err != nil {
		return fmt.Errorf("read embedded dir %q: %w", embedPath, err)
	}
	for _, e := range entries {
		childEmbedPath := embedPath + "/" + e.Name()
		if e.IsDir() {
			childFd, err := openOrCreateDirNoSymlink(dirFd, e.Name())
			if err != nil {
				return fmt.Errorf("dir %q: %w", e.Name(), err)
			}
			err = deploySkillDir(sourceFS, childFd, childEmbedPath)
			_ = unix.Close(childFd)
			if err != nil {
				return err
			}
			continue
		}

		embedded, err := fs.ReadFile(sourceFS, childEmbedPath)
		if err != nil {
			return fmt.Errorf("read embedded %q: %w", childEmbedPath, err)
		}
		unchanged, err := fileMatches(dirFd, e.Name(), embedded)
		if err != nil {
			return fmt.Errorf("check existing %q: %w", e.Name(), err)
		}
		if unchanged {
			continue
		}
		if err := writeFileSafeAt(dirFd, e.Name(), embedded, 0o644); err != nil {
			return fmt.Errorf("write %q: %w", e.Name(), err)
		}
	}
	return nil
}

// fileMatches reports whether the file already at name directly under
// dirFd exists and its content equals embedded, so deploySkillDir can skip
// a rewrite — matching DeployAll's pre-existing "only written when content
// differs" contract. A missing file is reported as (false, nil): the caller
// falls through to a normal write. A name that turns out to be a symlink is
// a hard error, same as everywhere else in this package — reading through
// an attacker-placed symlink to decide whether to skip a write is
// lower-severity than the write-side attack the Blocker was about, but
// there's no reason to special-case this read differently from the rest of
// the symlink-safe path.
func fileMatches(dirFd int, name string, embedded []byte) (bool, error) {
	f, exists, err := openFileNoSymlinkIfExists(dirFd, name)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	defer f.Close()
	existing, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("read %q: %w", name, err)
	}
	return bytes.Equal(existing, embedded), nil
}

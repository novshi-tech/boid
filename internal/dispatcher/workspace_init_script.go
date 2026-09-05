package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// The write side of the workspace init.sh, so `boid workspace
// set-init-script` (and the `init_script` field of a Workspace envelope)
// can put a script where the daemon will read it — the daemon runs in its
// own container, so the file must be written through its HTTP API rather
// than edited on the host.
//
// The store lives here rather than in internal/api because this package is
// where the file is READ (resolveWorkspaceHome / WorkspaceInitScriptStore.Path
// share the same path-resolving function, pinned by
// TestWorkspaceInitScriptStore_PathMatchesTheOneDispatchReads, so the two
// can't silently diverge). Policy — size cap, NUL-byte refusal, ETag/If-Match
// concurrency, "empty means no script" — stays in internal/api.

// WorkspaceInitScriptStore reads, writes and removes a workspace's init.sh at
// the path resolveWorkspaceHome reads it from.
//
// It has no fields on purpose: the reader takes its root from the ambient
// environment (os.UserConfigDir), so an injectable root here could point a
// writer somewhere the reader never looks. Tests isolate the environment
// instead (testutil/homeenv), matching what production does.
type WorkspaceInitScriptStore struct{}

// Path returns the absolute path of slug's init.sh on the daemon's
// filesystem, or an error when slug is not a valid workspace slug.
//
// Exposed so callers can tell an operator exactly where the file lives —
// under the container deploy that's a path inside the daemon's own volume,
// not something reachable by editing a file on the host.
func (WorkspaceInitScriptStore) Path(slug string) (string, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return "", err
	}
	return workspaceInitScriptPath(slug)
}

// Read returns slug's init.sh, reporting exists=false (with no error) when
// the workspace has none — that is a pass-through case, not a fault.
func (s WorkspaceInitScriptStore) Read(slug string) (data []byte, exists bool, err error) {
	path, err := s.Path(slug)
	if err != nil {
		return nil, false, err
	}
	return readIfExists(path)
}

// Write publishes data as slug's init.sh, creating the per-workspace config
// directory if needed.
//
// The file is 0600 and the directory 0700 — the daemon is the only reader,
// and the content is operator-authored shell that runs with the workspace's
// full network access. The directory's mode is set on every write, not just
// at creation, because os.MkdirAll only applies its mode to directories it
// creates — an install with a pre-existing, more permissive directory could
// otherwise let anyone in the group swap in their own init.sh. A chmod
// failure fails the write rather than publishing a script this function
// can no longer vouch for.
//
// It is deliberately not executable: boid never execs this file, it hashes
// the bytes and streams them into the init container for `bash` to run
// (buildWorkspaceInitScript), so an executable bit or shebang would imply a
// way of running it that doesn't exist.
//
// The publish is atomic (temp file, chmod, fsync, rename) so a concurrent
// resolveWorkspaceHome — which reads this path with no lock on the fast
// path — can never observe a half-written script.
func (s WorkspaceInitScriptStore) Write(slug string, data []byte) error {
	path, err := s.Path(slug)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create workspace config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict workspace config dir %s to 0700: %w", dir, err)
	}
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Remove deletes slug's init.sh, reporting whether there was one to delete.
//
// The per-workspace directory is left in place: it is cheap, and removing it
// would race a concurrent Write that has already created it.
func (s WorkspaceInitScriptStore) Remove(slug string) (removed bool, err error) {
	path, err := s.Path(slug)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove %s: %w", path, err)
	}
	return true, nil
}

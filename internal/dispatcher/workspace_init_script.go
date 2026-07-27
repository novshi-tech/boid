package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d): the
// write side of the workspace init.sh, so `boid workspace set-init-script`
// (and the `init_script` field of a Workspace envelope) can put a script
// where the daemon will read it.
//
// # Why a write path had to exist at all
//
// workspaceInitScriptPath resolves through os.UserConfigDir on the DAEMON.
// Before the volume-only cutover the daemon was a host process, so
// "~/.config/boid/workspaces/<slug>/init.sh" named a file the operator could
// open in an editor. It no longer does: the daemon runs in its own container
// and that path resolves inside its state volume, which nothing on the host
// can reach. The dogfood run this plan doc came out of had to `podman cp` the
// scripts in by hand — the same gap `boid config get/set/apply/edit` closed
// for config.yaml (docs/plans/volume-only-daemon.md §論点 f: a file the daemon
// owns is read and written through its HTTP API).
//
// # Why the store lives HERE and not in internal/api
//
// Because this package is where the file is READ. resolveWorkspaceHome hands
// workspaceInitScriptPath's result to readIfExists twice per init (once for
// the fast path, once under the lock). A writer that derived the same layout
// independently would be a second copy of a fact, and the failure mode of
// those two copies disagreeing is silent: the API reports success, the file
// lands somewhere real, and dispatch keeps running the workspace as if it had
// no init.sh at all. WorkspaceInitScriptStore.Path calls the same function,
// which is what TestWorkspaceInitScriptStore_PathMatchesTheOneDispatchReads
// pins.
//
// Everything policy-shaped — the size cap, the NUL-byte refusal, ETag/If-Match
// concurrency, and "an empty script means no script" — stays in internal/api,
// the same split 論点 a-2's D2 made for the HOME volume store: mechanism here,
// policy where the workspace rows are.

// WorkspaceInitScriptStore reads, writes and removes a workspace's init.sh at
// the path resolveWorkspaceHome reads it from.
//
// It is a struct with no fields on purpose. An injectable root would be the
// usual way to make a filesystem store testable, and it would break the one
// property this type exists for: the reader takes its root from the ambient
// environment (os.UserConfigDir), so a writer with its own configurable root
// could be pointed somewhere the reader never looks. Tests isolate the
// environment instead (testutil/homeenv), which is the same thing production
// does and therefore cannot drift from it.
type WorkspaceInitScriptStore struct{}

// Path returns the absolute path of slug's init.sh on the DAEMON's
// filesystem, or an error when slug is not a valid workspace slug.
//
// Exposed (rather than kept internal to the store's own methods) because it is
// worth showing an operator: under the container deploy the answer is a path
// inside the daemon's volume, and saying so is how a message stops implying
// that editing a file on the host would do anything. See the adapters'
// missingCLIError for the message that used to imply exactly that.
func (WorkspaceInitScriptStore) Path(slug string) (string, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return "", err
	}
	return workspaceInitScriptPath(slug)
}

// Read returns slug's init.sh, reporting exists=false (with no error) when the
// workspace has none — the pass-through class of
// docs/plans/home-workspace-volume.md 「script が無い workspace は素通し」, not a
// fault.
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
// The file is 0600 and the directory 0700 — the daemon is the only reader, and
// the content is operator-authored shell that runs with the workspace's full
// (unrestricted, see the guide) network access.
//
// The directory's mode is SET on every write, not merely requested at creation
// (PR9 codex round 2, Minor 4). os.MkdirAll applies its mode only to
// directories it creates, so an install that already had
// ~/.config/boid/workspaces/<slug>/ kept whatever mode the pre-PR9 hand-managed
// workflow left — 0775, group-writable, on the install this plan doc migrates.
// The file's own 0600 is not an answer to that: it protects the content, while
// a group-writable directory lets anyone in the group unlink init.sh and leave
// their own in its place, to be run by the workspace's init container with that
// workspace's network access and credentials. A chmod failure fails the write
// rather than being logged past — this tree is the daemon's own
// $XDG_CONFIG_HOME, so being unable to set the mode on it is a fault the
// operator needs told, and publishing the script anyway would leave it sitting
// somewhere this function just established it cannot vouch for.
//
// It is deliberately NOT executable. boid never execs the configured file: it
// reads the bytes, hashes them into the completion marker, and streams them
// into the init container inside a quoted heredoc for `bash` to run
// (buildWorkspaceInitScript). An executable bit would advertise a way of
// running this file that does not exist, and a shebang in it is ignored for
// the same reason.
//
// The publish is atomic (temp file in the same directory, chmod, fsync,
// rename) so a concurrent resolveWorkspaceHome — which reads this path with no
// lock on the fast path — can never observe a half-written script and hash it
// into a marker.
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

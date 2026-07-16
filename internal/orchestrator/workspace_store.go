package orchestrator

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkspaceStore provides persistence for WorkspaceMeta values.
//
// Two backing modes (docs/plans/workspace-db-consolidation.md PR3 cutover):
//
//   - repo == nil: legacy yaml mode. Each workspace is read/written as a
//     YAML file at <dir>/<slug>.yaml. This is the pre-cutover behavior, kept
//     as-is for callers that never wire a repository — e.g. the CLI
//     (cmd/workspace.go and friends still read/write the yaml files
//     directly; PR4 switches them to the API) and
//     MigrateWorkspaceYAMLToDB's own preflight, which needs to read the
//     legacy yaml as the migration's source of truth.
//   - repo != nil (set via SetRepository / NewWorkspaceStoreWithRepo): DB
//     mode. Every method delegates to repo instead of touching dir. dir is
//     retained on the struct only as the (now-shadow) yaml location; PR3
//     does not delete or read it once a repository is wired.
//
// Every method's signature is unchanged from the yaml-only era so existing
// callers (cmd/workspace.go, cmd/project_migrate.go, cmd/kit.go,
// cmd/kit_cleanup.go, dispatcher.WorkspaceLookup) need zero code changes —
// only daemon wiring (internal/server/wire.go) opts into DB mode by calling
// SetRepository.
type WorkspaceStore struct {
	dir  string
	repo *WorkspaceRepository
}

// NewWorkspaceStore returns a yaml-mode WorkspaceStore backed by dir.
// If dir is empty, DefaultWorkspaceDir is called to determine the directory.
// The directory is not created eagerly; it is created on the first Save call.
// Call SetRepository afterward (or use NewWorkspaceStoreWithRepo) to switch
// this store into DB mode.
func NewWorkspaceStore(dir string) *WorkspaceStore {
	if dir == "" {
		d, err := DefaultWorkspaceDir()
		if err != nil {
			// Fall back to an empty string so callers fail clearly on use.
			d = ""
		}
		dir = d
	}
	return &WorkspaceStore{dir: dir}
}

// NewWorkspaceStoreWithRepo returns a DB-mode WorkspaceStore: dir is kept
// only as the shadow yaml location, and every method delegates to repo.
func NewWorkspaceStoreWithRepo(dir string, repo *WorkspaceRepository) *WorkspaceStore {
	s := NewWorkspaceStore(dir)
	s.repo = repo
	return s
}

// SetRepository wires repo as this store's DB backing
// (docs/plans/workspace-db-consolidation.md PR3 cutover). Once set, every
// method routes through repo instead of the yaml directory.
func (s *WorkspaceStore) SetRepository(repo *WorkspaceRepository) {
	s.repo = repo
}

// DefaultWorkspaceDir returns the default directory for workspace YAML files:
// $XDG_CONFIG_HOME/boid/workspaces, or ~/.config/boid/workspaces when
// XDG_CONFIG_HOME is unset (matching the behaviour of os.UserConfigDir on Linux).
func DefaultWorkspaceDir() (string, error) {
	// os.UserConfigDir already follows XDG_CONFIG_HOME on Linux and falls
	// back to $HOME/.config when the variable is unset.
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("workspace store: could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "workspaces"), nil
}

// Load reads and parses the WorkspaceMeta for the given slug.
// Returns an error wrapping os.ErrNotExist when the file does not exist.
//
// In DB mode (repo != nil) a not-found row falls back to the yaml file at
// <dir>/<slug>.yaml (docs/plans/workspace-db-consolidation.md 「rollback
// 用に旧 workspace yaml は残す」): PR3's cutover only migrates yaml files
// present at daemon-startup time — anything a user (or an e2e scenario)
// drops into the workspaces dir *after* startup never made it into the DB
// migration, and the daemon has no post-cutover create path yet (that is
// PR4). Rather than let those workspaces silently run in degraded mode
// ("workspace.yaml not found" warnings + capabilities/kits/env not
// injected + hooks failing with `command not found` because kit env is
// missing), we transparently fall back to reading the yaml file when the
// DB row is absent. PR4 wires the proper POST /api/workspaces path and
// migrates every yaml on write; at that point this fallback becomes
// unnecessary and will be removed alongside the CLI cutover.
func (s *WorkspaceStore) Load(slug string) (*WorkspaceMeta, error) {
	if s.repo != nil {
		meta, err := s.repo.Load(slug)
		if err == nil {
			return meta, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// DB row missing — fall through to the yaml path below. Callers
		// that only care about the DB view (workspace_migration.go's own
		// preflight) construct a store with dir set to the yaml dir
		// they already want to read, so the fallback there is
		// intentional too.
	}
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, err
	}
	if s.dir == "" {
		return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
	}
	path := filepath.Join(s.dir, slug+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workspace %q (%s): %w", slug, path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("workspace %q (%s): read: %w", slug, path, err)
	}
	var meta WorkspaceMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("workspace %q (%s): parse: %w", slug, path, err)
	}
	return &meta, nil
}

// Save atomically writes meta as YAML to <dir>/<slug>.yaml.
// The directory is created with mode 0755 if it does not exist.
// The write is atomic: the data is first written to a temporary file in the
// same directory and then renamed into place.
func (s *WorkspaceStore) Save(slug string, meta *WorkspaceMeta) error {
	if s.repo != nil {
		return s.repo.Save(slug, meta)
	}
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("workspace store: mkdir %q: %w", s.dir, err)
	}
	data, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("workspace %q: marshal: %w", slug, err)
	}

	// Write to a temporary file in the same directory, then rename atomically.
	tmpName := filepath.Join(s.dir, fmt.Sprintf("%s.yaml.tmp.%08x", slug, rand.Uint32()))
	if err := os.WriteFile(tmpName, data, 0o644); err != nil {
		return fmt.Errorf("workspace %q: write tmp: %w", slug, err)
	}
	dest := filepath.Join(s.dir, slug+".yaml")
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("workspace %q: rename: %w", slug, err)
	}
	return nil
}

// Remove deletes the YAML file for the given slug.
// Returns an error wrapping os.ErrNotExist when the file does not exist.
// The reserved DefaultWorkspaceSlug cannot be removed.
// It does not check whether any project references this workspace.
func (s *WorkspaceStore) Remove(slug string) error {
	if s.repo != nil {
		return s.repo.Remove(slug)
	}
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if slug == DefaultWorkspaceSlug {
		return fmt.Errorf("workspace %q is reserved and cannot be removed", slug)
	}
	path := filepath.Join(s.dir, slug+".yaml")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return fmt.Errorf("workspace %q: remove: %w", slug, err)
	}
	return nil
}

// EnsureDefault writes an empty WorkspaceMeta to the DefaultWorkspaceSlug
// path if no file exists yet. It is safe to call repeatedly and a no-op once
// the file exists (the existing content — including user edits — is left
// untouched). Returns nil when the workspace dir cannot be determined yet
// (DefaultWorkspaceDir failure): callers are expected to surface that via
// the first Load/Save attempt rather than at boot.
func (s *WorkspaceStore) EnsureDefault() error {
	if s.repo != nil {
		return s.repo.EnsureDefault()
	}
	if s.dir == "" {
		return fmt.Errorf("workspace store: dir not configured")
	}
	path := filepath.Join(s.dir, DefaultWorkspaceSlug+".yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace %q: stat: %w", DefaultWorkspaceSlug, err)
	}
	return s.Save(DefaultWorkspaceSlug, &WorkspaceMeta{})
}

// List returns the slug of every workspace stored in the directory, sorted
// alphabetically. If the directory does not exist, an empty slice and nil
// error are returned (degraded window).
func (s *WorkspaceStore) List() ([]string, error) {
	if s.repo != nil {
		return s.repo.List()
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("workspace store: list %q: %w", s.dir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		slug := strings.TrimSuffix(name, ".yaml")
		// Skip files whose base name is not a valid slug (e.g. tmp files).
		if ValidWorkspaceSlug(slug) == nil {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

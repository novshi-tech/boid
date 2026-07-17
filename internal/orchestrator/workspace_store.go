package orchestrator

import (
	"errors"
	"fmt"
	"log/slog"
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
// In DB mode (repo != nil) this delegates straight to repo.Load and never
// falls back to the yaml file at <dir>/<slug>.yaml. PR3's cutover had a
// transitional yaml fallback here (a not-found DB row fell back to reading
// the legacy yaml file) because the daemon had no post-cutover create path
// yet — a workspace yaml dropped into the workspaces dir *after* daemon
// startup (missed by MigrateWorkspaceYAMLToDB's one-time migration) would
// otherwise silently degrade (capabilities/kits/env not injected). PR4
// removes that fallback now that POST /api/workspaces (and
// cmd/workspace.go's `boid workspace assign` auto-create) gives every
// caller a real way to introduce a DB row outside the migration path — the
// yaml files under DefaultWorkspaceDir() are now purely a rollback/export
// shadow (decision 16), never read by DB-mode Load again.
func (s *WorkspaceStore) Load(slug string) (*WorkspaceMeta, error) {
	if s.repo != nil {
		return s.repo.Load(slug)
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
	// Codex Should-fix (PR4 review, docs/plans/home-workspace-volume.md):
	// WorkspaceMeta has no AdditionalBindings field any more (Phase 4 PR4),
	// so the yaml.Unmarshal above silently drops an additional_bindings:
	// key with no diagnostic at all — unlike the wire (POST/PUT) path,
	// which already warns via workspaceMetaStrict.toWorkspaceMeta. The plan
	// requires "parse continues + ignore + warn" for every legacy read
	// path, so this yaml-mode load path needs the same warning. Errors from
	// additionalBindingsKeyPresent are swallowed deliberately: data already
	// decoded successfully into WorkspaceMeta above, so a failure here would
	// only ever be a best-effort diagnostic miss, never a reason to fail
	// this Load.
	if present, _ := additionalBindingsKeyPresent(data); present {
		slog.Warn("workspace: additional_bindings is no longer supported (retired in docs/plans/home-workspace-volume.md Phase 4 PR4); ignoring",
			"workspace", slug, "path", path)
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

// LoadWithRevision reads meta and its revision from a single atomic
// snapshot in DB mode (docs/plans/workspace-db-consolidation.md MAJOR 1,
// codex review) — see WorkspaceRepository.LoadWithRevision's doc comment
// for why this matters. Yaml mode (repo == nil) has no revision concept, so
// it falls back to plain Load with an empty revision string.
func (s *WorkspaceStore) LoadWithRevision(slug string) (*WorkspaceMeta, string, error) {
	if s.repo != nil {
		return s.repo.LoadWithRevision(slug)
	}
	meta, err := s.Load(slug)
	if err != nil {
		return nil, "", err
	}
	return meta, "", nil
}

// UpdateIfRevisionMatches performs the CAS update described on
// WorkspaceRepository.UpdateIfRevisionMatches in DB mode. Yaml mode (repo ==
// nil) has no revision concept to enforce — it always reports matched=true
// and performs an unconditional Save, i.e. the same non-CAS overwrite
// semantics this mode's plain Save has always had.
func (s *WorkspaceStore) UpdateIfRevisionMatches(slug string, expectedRevision string, meta *WorkspaceMeta) (newRevision string, matched bool, err error) {
	if s.repo != nil {
		return s.repo.UpdateIfRevisionMatches(slug, expectedRevision, meta)
	}
	if err := s.Save(slug, meta); err != nil {
		return "", false, err
	}
	return "", true, nil
}

// Create writes a brand-new workspace at slug, insert-only: a slug that
// already exists (as a DB row in DB mode, or a yaml file in yaml mode) is
// rejected with an error wrapping os.ErrExist rather than silently
// overwritten (docs/plans/workspace-db-consolidation.md Step A — the API's
// POST /api/workspaces create endpoint relies on this to return HTTP 409).
func (s *WorkspaceStore) Create(slug string, meta *WorkspaceMeta) error {
	if s.repo != nil {
		return s.repo.Create(slug, meta)
	}
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if s.dir == "" {
		return fmt.Errorf("workspace store: dir not configured")
	}
	path := filepath.Join(s.dir, slug+".yaml")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("workspace %q: %w", slug, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace %q: stat: %w", slug, err)
	}
	return s.Save(slug, meta)
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

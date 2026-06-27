package orchestrator

import (
	"fmt"
)

// ProjectMissingError is a typed error indicating that a project registered
// in the DB no longer has a project.yaml on disk (the project directory was
// removed, the path moved, etc.). It is distinct from
// *ProjectMigrationError, which signals schema migration is required.
//
// Callers (boid start parent / server wire) extract this via errors.As and
// auto-prune the stale row from the project DB so the daemon can boot
// instead of refusing because of a dangling registration. The decision is
// data-safe: the project.yaml is the source of truth, so a DB row that
// points at a vanished directory is unambiguously stale.
type ProjectMissingError struct {
	ProjectID string // registered project ID
	Dir       string // expected project root (where .boid/project.yaml should live)
	Err       error  // underlying os.ReadFile error (preserved for diagnostics / errors.Is)
}

// Error matches the legacy `project "<id>": <inner>` shape used by
// wrapPerProjectLoadErr so existing log output is byte-identical.
func (e *ProjectMissingError) Error() string {
	if e == nil {
		return "project missing error"
	}
	return fmt.Sprintf("project %q: %s", e.ProjectID, e.Err.Error())
}

// Unwrap exposes the underlying os.ReadFile error so callers can
// errors.Is(err, fs.ErrNotExist) without knowing about ProjectMissingError.
func (e *ProjectMissingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

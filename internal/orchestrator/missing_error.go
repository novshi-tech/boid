package orchestrator

import (
	"fmt"
)

// ProjectMissingError is a typed error indicating that a project registered
// in the DB failed to load its project.yaml. It is distinct from
// *ProjectMigrationError, which signals schema migration is required.
//
// wrapPerProjectLoadErr (project_store.go) returns this type for every
// per-project load failure that is not a *ProjectMigrationError — a missing
// directory, a YAML parse error, a corrupt/unreachable bare repo, all of
// it — because filesystem/remote observations never justify a hard delete:
// internal/server/wire.go's buildProjectStore marks the project "degraded"
// (ProjectStore.MarkDegraded) and keeps going instead. The DB row is only
// ever removed by an explicit `boid project rm` / `boid workspace delete`.
// The type name stays as-is despite no longer being specific to the
// "missing" case.
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

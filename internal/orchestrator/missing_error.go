package orchestrator

import (
	"fmt"
)

// ProjectMissingError is a typed error indicating that a project registered
// in the DB failed to load its project.yaml. It is distinct from
// *ProjectMigrationError, which signals schema migration is required.
//
// Originally (pre docs/plans/volume-only-daemon.md) this covered only the
// "project directory removed from disk" case, and callers auto-pruned the
// DB row on sight — safe because the project.yaml was the source of truth,
// so a row pointing at a vanished directory was unambiguously stale.
//
// docs/plans/volume-only-daemon.md §論点a retired that auto-prune: the
// pivot-triggering incident it documents was caused by exactly this
// short-circuit firing on a false "missing" signal (host filesystem
// temporarily unreachable from inside a container). The declared invariant
// is now: filesystem/remote observations never justify a hard delete, at
// any point (startup / dispatch / fetch / GC). wrapPerProjectLoadErr
// (project_store.go) now returns this type for EVERY per-project load
// failure that is not a *ProjectMigrationError — a missing directory, a
// YAML parse error, a corrupt/unreachable bare repo (project_bare_repo.go),
// all of it — because as of the invariant above they all get the same
// response: internal/server/wire.go's buildProjectStore marks the project
// "degraded" (ProjectStore.MarkDegraded) and keeps going. The DB row is
// only ever removed by an explicit `boid project rm` / `boid workspace
// delete`. The type name stays as-is (not worth the rename-only churn
// across call sites) despite no longer being specific to the "missing"
// case.
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

package orchestrator

import (
	"fmt"
	"strings"
)

// ProjectMigrationIssue describes one project whose project.yaml uses the
// pre-Phase-3-e schema (top-level kits / host_commands / additional_bindings /
// secret_namespace / capabilities, or task_behaviors.<name>.kits) and must be
// migrated to the new workspace+kit layout via `boid project migrate`.
type ProjectMigrationIssue struct {
	// ProjectID is the registered project ID, when known. Empty when the
	// issue surfaces from a loader path that does not yet have a DB lookup
	// (e.g. ReadProjectMeta called directly with a path).
	ProjectID string
	// Dir is the absolute path to the project root (parent of .boid/).
	Dir string
	// Messages are the per-field violation messages in the same order as
	// the old single-string error produced by rejectRemovedProjectFields.
	Messages []string
}

// ProjectMigrationError is a typed error carrying one or more migration
// issues. Callers (e.g. boid start) use errors.As to extract the issues and
// drive auto-migration; Error() preserves the legacy multi-line format so
// log output (boid.log) is byte-identical to the pre-typed-error version.
type ProjectMigrationError struct {
	Projects []ProjectMigrationIssue
}

// Error returns the multi-line, human-readable error message. The format
// for a single issue is byte-identical to the legacy
// rejectRemovedProjectFields output so existing spec_loader tests pass
// unchanged. Multiple issues are separated by a single newline.
func (e *ProjectMigrationError) Error() string {
	if e == nil || len(e.Projects) == 0 {
		return "project migration error: no issues"
	}
	parts := make([]string, 0, len(e.Projects))
	for _, p := range e.Projects {
		parts = append(parts, FormatMigrationIssue(p))
	}
	return strings.Join(parts, "\n")
}

// FormatMigrationIssue renders a single ProjectMigrationIssue using the
// canonical multi-line format. When ProjectID is non-empty the output is
// prefixed with `project "<ID>": ` to mirror the wrapping previously done
// by project_store.LoadAll via fmt.Errorf("project %q: %w", id, err).
//
// Exported so that callers building aggregate messages (server/wire.go,
// boid start parent) can reuse the same canonical format for each issue.
func FormatMigrationIssue(p ProjectMigrationIssue) string {
	combined := strings.Join(p.Messages, "\n")
	guidance := migrationGuidance(p.Dir)
	body := combined + "\n" + guidance
	if p.ProjectID != "" {
		return fmt.Sprintf("project %q: %s", p.ProjectID, body)
	}
	return body
}

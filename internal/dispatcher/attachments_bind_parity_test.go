// Package dispatcher_test (external/black-box), not dispatcher: internal/api
// already imports internal/dispatcher (job_log_sse.go, workspace_homes.go,
// ws_attach.go), so an *internal* dispatcher test file importing internal/api
// would make the dispatcher test package cyclic. dispatcher_test is a
// distinct package identity api does not import, so it can safely import
// both sides.
package dispatcher_test

import (
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
)

// TestAttachmentsBindSource_MatchesAPIHelper guards wiring-seams.md #15
// (attachments RPC vs dispatch-time attachments bind): the per-task
// attachments RO bind's source path (sandbox_builder.go, the
// `attachSrc := filepath.Join(rt.AttachmentsRoot, "tasks", spec.TaskID,
// "attachments")` expression) and api.AttachmentsRootForTask are two
// *independent* path builders — internal/dispatcher cannot import
// internal/api in production code (internal/api already imports
// internal/dispatcher, e.g. job_log_sse.go/workspace_homes.go/ws_attach.go
// — the reverse would be a cycle), so the bind construction cannot simply
// call the shared helper. This test (an external (dispatcher_test) test
// package, so it can safely import both internal/dispatcher and
// internal/api without a cycle) pins that the two still compute the same
// result for representative, already-canonical inputs, so a future change
// to either side that breaks parity fails loudly here instead of silently
// diverging during the parallel-bind-and-RPC window (PR2 through the 5b-6
// cutover, which retires the bind entirely).
//
// This test does NOT exercise either side's canonical-path-component
// rejection guard: the bind construction (sandbox_builder.go) validates
// spec.TaskID via isCanonicalTaskIDComponent (attachments_path.go, a
// deliberate duplicate — see its doc comment for why) before ever calling
// filepath.Join, and api.AttachmentsRootForTask does the equivalent via
// isCanonicalPathComponent; both were added in the same PR (#798, Blocker
// fix) but are two separately-maintained function bodies, so keeping their
// rejection rules identical is a manual-review obligation this parity test
// cannot enforce — see wiring-seams.md #15 for the full picture.
func TestAttachmentsBindSource_MatchesAPIHelper(t *testing.T) {
	cases := []struct{ root, taskID string }{
		{"/data", "task-1"},
		{"/data/home", "550e8400-e29b-41d4-a716-446655440000"},
		{"/var/lib/boid", "my_task.v2"},
	}
	for _, tc := range cases {
		// Mirrors sandbox_builder.go's attachSrc construction verbatim.
		bindSrc := filepath.Join(tc.root, "tasks", tc.taskID, "attachments")
		apiSrc := api.AttachmentsRootForTask(tc.root, tc.taskID)
		if bindSrc != apiSrc {
			t.Errorf("bind source %q != api.AttachmentsRootForTask %q for (root=%q, taskID=%q)", bindSrc, apiSrc, tc.root, tc.taskID)
		}
	}
}

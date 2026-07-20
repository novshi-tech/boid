package dispatcher

import (
	"path/filepath"
	"strings"
)

// isCanonicalTaskIDComponent mirrors internal/api's isCanonicalPathComponent
// (internal/api/attachments.go) for the one call site in this package that
// builds an attachments filesystem path: the per-task attachments RO bind's
// source directory in sandbox_builder.go. internal/dispatcher cannot import
// internal/api to call the original directly — internal/api already imports
// internal/dispatcher (job_log_sse.go, workspace_homes.go, ws_attach.go), so
// the reverse import would be a cycle — so this is a deliberate, minimal
// duplication of the validation logic rather than a shared call.
//
// codex review on PR #798 (Phase 5b PR2 attachments RPCs) flagged the
// Blocker this closes: CreateTaskRequest.ID is caller-supplied and saved as
// the literal DB primary key without validation, so a task can be
// dispatched with a traversal-shaped literal ID such as
// "alias/../<victim-id>". Without this guard, the bind's bare
// filepath.Join(root, "tasks", taskID, "attachments") silently collapsed
// such an ID down to the *victim* task's real attachments directory
// (filepath.Join normalizes ".." segments) — the sandbox for an unrelated
// task would see another task's attachments RO-bound at
// ~/.boid/attachments. The RPC read/write paths were fixed the same way in
// api.AttachmentsRootForTask, in the same PR; this closes the bind, the
// third of the three independent path-construction call sites documented
// in wiring-seams.md #14.
//
// Keep this in lock-step with api.isCanonicalPathComponent — there is no
// automated cross-package drift guard for the *rejection rule* itself (only
// TestAttachmentsBindSource_MatchesAPIHelper, which pins that the two
// happen to compute the same *path* for ordinary, already-canonical
// inputs); a reviewer changing either definition must manually update both.
func isCanonicalTaskIDComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsRune(s, filepath.Separator) {
		return false
	}
	return filepath.Base(filepath.Clean(s)) == s
}

package apiwire

// ReleaseResult is the response shape for
// POST /api/card-requests/{id}/release: it echoes the request's pre-release
// target (if any) since force-release only frees the slot, not whatever
// continuation was still attached to it.
type ReleaseResult struct {
	Status     string `json:"status"`
	TargetKind string `json:"target_kind,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	// LauncherJobID is set alongside OperatorNotice for a released row that
	// was still "launching" — its own launcher job, not a task/session
	// continuation, is the thing that may still be running.
	LauncherJobID string `json:"launcher_job_id,omitempty"`
	// HadAttachedTarget reports only that the pre-release row HAD a
	// target_kind/target_id recorded — not that the target is still alive.
	// A task/session that already reached a terminal state and is merely
	// awaiting the next reconcile tick to clear the row also sets this true.
	HadAttachedTarget bool `json:"had_attached_target,omitempty"`
	// FoldedSiblingsFailed lists every OTHER card_requests row this release
	// also force-failed (fold is card-scoped, not this request's own
	// command_key/cause_id).
	FoldedSiblingsFailed []FoldedSiblingSummary `json:"folded_siblings_failed,omitempty"`
	OperatorNotice       string                 `json:"operator_notice,omitempty"`
}

// FoldedSiblingSummary is one row FoldedSiblingsFailed reports.
type FoldedSiblingSummary struct {
	ID         string `json:"id"`
	CommandKey string `json:"command_key,omitempty"`
}

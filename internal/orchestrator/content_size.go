package orchestrator

import "fmt"

// MaxContentBytes bounds `description` and action payload content: content
// exceeding it is rejected with an error rather than silently truncated,
// since a head/agent-originated source has no external original to recover
// from a truncation. 64 KiB gives roughly 4.2x margin over measured
// description content and 2.9x over measured action payload content — see
// docs/plans/ingestion-identity.md §13 A-5 for the full measurement data and
// the rationale for sharing one constant across both fields.
//
// Applied at every content-entry point (task_create/task_update/
// BoidOpTaskResolveOrCapture for description; action_send and
// task_notify.go's progress/done_request/fail_request for action payload) —
// any entry point that skips ValidateContentSize is a bypass, so a new
// write path must always call it.
const MaxContentBytes = 64 * 1024 // 65,536 bytes

// ContentSizeError is ValidateContentSize's error type. Exported so a caller
// that wants the raw byte counts (rather than parsing Error()'s text) can use
// errors.As — the message text itself is not a public contract.
type ContentSizeError struct {
	Field       string
	ActualBytes int
	LimitBytes  int
}

func (e *ContentSizeError) Error() string {
	return fmt.Sprintf("%s exceeds the size limit: %d bytes (limit %d bytes)", e.Field, e.ActualBytes, e.LimitBytes)
}

// ValidateContentSize rejects content whose byte length exceeds
// MaxContentBytes. field names the content in the returned error (e.g.
// "description", "action payload") so a caller can tell which limit was hit.
func ValidateContentSize(field string, content []byte) error {
	if len(content) <= MaxContentBytes {
		return nil
	}
	return &ContentSizeError{Field: field, ActualBytes: len(content), LimitBytes: MaxContentBytes}
}

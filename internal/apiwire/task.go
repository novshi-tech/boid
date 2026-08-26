package apiwire

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type NotifyTaskRequest struct {
	Message    string `json:"message"`
	Ask        string `json:"ask,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	Progress   string `json:"progress,omitempty"`
	Done       string `json:"done,omitempty"`
	Fail       string `json:"fail,omitempty"`
}

type AnswerTaskRequest struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

type UpdateTaskRequest struct {
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	ProjectID    string          `json:"project_id,omitempty"`
	RemoteID     *string         `json:"remote_id,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Instructions json.RawMessage `json:"instructions,omitempty"`
	ParentID     *string         `json:"parent_id,omitempty"`
	AutoStart    *bool           `json:"auto_start,omitempty"`
}

type CreateTaskRequest struct {
	ID           string                     `json:"id,omitempty"`
	ProjectID    string                     `json:"project_id"`
	Title        string                     `json:"title"`
	Description  string                     `json:"description,omitempty"`
	Behavior     string                     `json:"behavior,omitempty"`
	BehaviorSpec *orchestrator.BehaviorSpec `json:"behavior_spec,omitempty"`
	RemoteID     string                     `json:"remote_id,omitempty"`
	Payload      json.RawMessage            `json:"payload,omitempty"`
	Instructions json.RawMessage            `json:"instructions,omitempty"`
	AutoStart    bool                       `json:"auto_start,omitempty"`
	Traits       []string                   `json:"traits,omitempty"`
	Ref          string                     `json:"ref,omitempty"`
	ParentID     string                     `json:"parent_id,omitempty"`
	Readonly     *bool                      `json:"readonly,omitempty"`
	// InitialStatus lets a caller create a task directly in a pre-execution
	// status (docs/plans/cross-project-issue-triage.md Phase 1 PR-1) instead
	// of the default "pending". Empty means "pending" (unchanged behavior).
	// Only "", "pending", "parked" are accepted — validated in
	// internal/api/task_create.go, not here (apiwire is a pure wire struct).
	// ("captured"/"triaged" were accepted pre-card-model-cleanup PR-2; card
	// machine v2 has no such statuses, a card is now born directly into
	// "parked" — see task_create.go's allowedCreateInitialStatuses.)
	InitialStatus string `json:"initial_status,omitempty"`
	// IdempotencyKey (docs/plans/signal-ingest-detailed-design.md §8): a
	// caller-supplied stable key, scoped by (ProjectID, ParentID), that makes
	// a create call safe to retry — a second call with the same (ProjectID,
	// ParentID, IdempotencyKey) returns the existing task instead of creating
	// a duplicate (exit 0, not an error). See migration 0047 and
	// orchestrator.Task.IdempotencyKey's doc comment for why ParentID is part
	// of the scope (a same-project, different-parent key collision must not
	// silently hand back the WRONG parent's child).
	//
	// Reachable via all three of:
	//   - the host CLI: `boid task create --idempotency-key <key>`
	//     (cmd/task.go's runTaskCreate)
	//   - this JSON field on POST /api/tasks directly
	//   - the sandboxed `task_create` builtin op, via EITHER `idempotency_key:`
	//     in the YAML spec (forwarded wholesale, no special-casing needed) OR
	//     its own `--idempotency-key` flag (internal/sandbox/boid_shim.go's
	//     parseBoidTaskCreate — this is the primary intended call site: a
	//     judgment task minting a child task runs inside the sandbox, so this
	//     flag form was NOT optional plumbing)
	//
	// See orchestrator.Task.IdempotencyKey's doc comment for the distinction
	// from task_identities/Ref (no external-identity or link/drop semantics
	// here — purely an internal dedup key).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type DuplicateTaskRequest struct {
	AutoStart bool `json:"auto_start"`
}

type RerunTaskRequest struct {
	AutoStart            bool            `json:"auto_start,omitempty"`
	InstructionsOverride json.RawMessage `json:"instructions_override,omitempty"`
}

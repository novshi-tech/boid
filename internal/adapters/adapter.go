// Package adapters defines the HarnessAdapter interface and shared types for
// harness-specific agent protocol implementations.
//
// Adapters live in internal/adapters/<harness>/. The boid core references only
// this package (the interface and Usage type); harness details stay inside each
// sub-package.
package adapters

import (
	"context"
	"encoding/json"
)

// Usage holds token consumption metrics for a completed job.
// Fixed fields cover the common denominator across harnesses; Extra stores
// harness-specific data without requiring schema migrations.
type Usage struct {
	// Model is the model identifier used for this job (e.g. "claude-opus-4-8").
	Model string `json:"model,omitempty"`

	// InputTokens is the number of uncached input tokens consumed.
	InputTokens int64 `json:"input_tokens"`

	// OutputTokens is the number of generated output tokens.
	OutputTokens int64 `json:"output_tokens"`

	// CacheCreationTokens is the number of tokens written to the prompt cache.
	// Zero for harnesses that do not support prompt caching.
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`

	// CacheReadTokens is the number of tokens served from the prompt cache.
	// Zero for harnesses that do not support prompt caching.
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`

	// Extra holds harness-specific data not captured by the fixed fields above.
	// Nil when no additional data is available.
	Extra json.RawMessage `json:"extra,omitempty"`
}

// HarnessAdapter abstracts harness-specific agent protocol from boid core.
// Each supported harness (claude, codex, opencode, …) provides one implementation.
// The boid core calls these methods without knowing which harness is in use.
type HarnessAdapter interface {
	// StopAgent asks the agent backing runtimeID to terminate gracefully,
	// leaving the surrounding bash runtime and EXIT trap alive so payload_patch
	// capture completes through the broker normally.
	StopAgent(ctx context.Context, runtimeID string) error

	// StopSignalName returns the bash signal name (e.g. "USR1") used in
	// `trap '' <name>` inside generated sandbox scripts. The outer and inner
	// bash scripts ignore this signal so that only the agent process itself
	// reacts when StopAgent delivers it to the process group.
	StopSignalName() string

	// ResumePayload returns argv flags and environment variables to pass to
	// the start hook when resuming an existing agent session identified by
	// sessionID.
	ResumePayload(sessionID string) (args []string, env map[string]string)

	// Interactive reports whether this harness requires a PTY allocation.
	Interactive() bool

	// SessionIDFromHookEnv extracts the harness session ID from env variables
	// delivered to a start hook. The inverse of the env produced by ResumePayload.
	SessionIDFromHookEnv(env map[string]string) string

	// Usage returns token consumption metrics for the job identified by jobID.
	// Returns a zero Usage and a nil error when metrics are not yet available.
	Usage(ctx context.Context, jobID string) (Usage, error)
}

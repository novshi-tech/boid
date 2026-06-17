package orchestrator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// BehaviorResolution holds the resolved behavior fields after processing either
// a named behavior or an inline behavior_spec.
type BehaviorResolution struct {
	BehaviorName string
	Traits       []string
	Readonly     bool
	Worktree     bool
	BranchPrefix string
	BaseBranch   string
	Payload      json.RawMessage
	Instructions Instructions
}

// DefaultBehavior is the hardcoded fallback behavior name used when a
// CreateTaskRequest omits both behavior and behavior_spec, the project meta is
// nil, and no default_task_behavior is configured. When meta is non-nil, the
// default resolution consults meta.DefaultTaskBehavior first, then falls back
// to "supervisor" with a deprecation warning if that behavior exists.
const DefaultBehavior = "supervisor"

// shouldWarnDeprecation reports whether deprecation warnings should be emitted.
// Suppressed by the BOID_NO_DEPRECATION_WARN=1 environment variable.
func shouldWarnDeprecation() bool {
	return os.Getenv("BOID_NO_DEPRECATION_WARN") != "1"
}

// BehaviorResolveRequest carries the behavior-relevant fields from a task creation request.
type BehaviorResolveRequest struct {
	Behavior     string
	BehaviorSpec *BehaviorSpec
	Payload      json.RawMessage
	Instructions json.RawMessage
}

// LookupBehaviorWithAlias finds a TaskBehavior in meta.TaskBehaviors by name,
// being tolerant of the plan / dev → supervisor / executor rename. Lookup is
// tried in this order:
//
//  1. exact match against the requested name
//  2. if the request is a legacy alias, try the canonical name
//  3. if the request is a canonical name, try the legacy alias (handles
//     unnormalized in-memory ProjectMeta values that may exist in tests or
//     transitional code paths)
//
// When (2) or (3) hits, a deprecation warning is logged. The returned key
// is the map key that actually matched; callers may use it for further
// logging or store the canonical form on the task.
func LookupBehaviorWithAlias(meta *ProjectMeta, name string) (TaskBehavior, string, bool) {
	if b, ok := meta.TaskBehaviors[name]; ok {
		return b, name, true
	}
	if canonical, isAlias := CanonicalBehaviorName(name); isAlias {
		if b, ok := meta.TaskBehaviors[canonical]; ok {
			slog.Warn("task behavior name is deprecated; use canonical name instead",
				"scope", "CreateTask request",
				"deprecated", name,
				"canonical", canonical,
			)
			return b, canonical, true
		}
	}
	// Reverse: caller used the new canonical name, but meta still uses the
	// alias key (legacy in-memory meta, e.g. hand-built test fixtures).
	for alias, canonical := range BehaviorAliases {
		if canonical != name {
			continue
		}
		if b, ok := meta.TaskBehaviors[alias]; ok {
			slog.Warn("project meta uses deprecated behavior name; please regenerate via ReadProjectMetaWithKits",
				"scope", "CreateTask request",
				"deprecated", alias,
				"canonical", name,
			)
			return b, alias, true
		}
	}
	return TaskBehavior{}, "", false
}

// ResolveBehavior validates and resolves behavior fields from a task creation request.
// It handles both the named behavior path (meta lookup) and the inline behavior_spec path.
// When both behavior and behavior_spec are empty, the default is resolved via:
//  1. meta.DefaultTaskBehavior if set
//  2. implicit "supervisor" fallback if that behavior exists in meta (with WARN)
//  3. error if neither is available
//  4. hardcoded DefaultBehavior when meta is nil (nil-meta paths, e.g. test wiring)
func ResolveBehavior(meta *ProjectMeta, req BehaviorResolveRequest) (*BehaviorResolution, error) {
	if req.Behavior != "" && req.BehaviorSpec != nil {
		return nil, fmt.Errorf("behavior and behavior_spec are mutually exclusive")
	}
	if req.Behavior == "" && req.BehaviorSpec == nil {
		if meta == nil {
			req.Behavior = DefaultBehavior
		} else if meta.DefaultTaskBehavior != "" {
			req.Behavior = meta.DefaultTaskBehavior
		} else if _, hasSupervisor := meta.TaskBehaviors["supervisor"]; hasSupervisor {
			if shouldWarnDeprecation() {
				slog.Warn("no default_task_behavior set; falling back to 'supervisor'. Set default_task_behavior in project.yaml to silence this warning.",
					"project_id", meta.ID)
			}
			req.Behavior = "supervisor"
		} else {
			return nil, fmt.Errorf("no default_task_behavior specified and no 'supervisor' behavior found in project %q", meta.ID)
		}
	}

	res := &BehaviorResolution{Payload: req.Payload}

	if req.BehaviorSpec != nil {
		spec := req.BehaviorSpec
		if spec.Name == "" {
			return nil, fmt.Errorf("behavior_spec.name is required")
		}
		res.BehaviorName = spec.Name
		res.Traits = spec.Traits
		if meta != nil {
			res.Worktree = meta.Worktree
			res.BaseBranch = meta.BaseBranch
		}
		// Inline specs have no TaskBehavior.Readonly field; pass nil → uses default.
		applyCanonicalBehaviorOverrides(res, meta, nil)
		mergedInstructions, err := MergeDefaultInstructions(spec.DefaultInstruction, req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions merge: %w", err)
		}
		res.Instructions = mergedInstructions
		return res, nil
	}

	// Named behavior path.
	res.BehaviorName = req.Behavior
	if meta != nil {
		behavior, lookupKey, ok := LookupBehaviorWithAlias(meta, req.Behavior)
		if !ok {
			return nil, fmt.Errorf("behavior %q not found", req.Behavior)
		}
		// When alias resolution kicked in (the meta key we matched differs
		// from what the caller asked for), persist the canonical form on
		// the task so rows converge regardless of which alias the caller
		// or the meta used. Exact matches are preserved verbatim to keep
		// legacy callers / fixtures stable until Phase 5.
		if lookupKey != req.Behavior {
			canonical, _ := CanonicalBehaviorName(req.Behavior)
			res.BehaviorName = canonical
		}
		res.Traits = behavior.Traits
		res.Worktree = meta.Worktree
		res.BaseBranch = meta.BaseBranch
		mergedInstructions, err := MergeDefaultInstructions(behavior.DefaultInstruction, req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions merge: %w", err)
		}
		res.Instructions = mergedInstructions

		applyCanonicalBehaviorOverrides(res, meta, behavior.Readonly)
	} else if len(req.Instructions) > 0 {
		mergedInstructions, err := MergeDefaultInstructions(nil, req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions merge: %w", err)
		}
		res.Instructions = mergedInstructions
		applyCanonicalBehaviorOverrides(res, nil, nil)
	} else {
		applyCanonicalBehaviorOverrides(res, nil, nil)
	}
	return res, nil
}

// applyCanonicalBehaviorOverrides sets res.Readonly using Track A2 semantics:
//
//  1. Default is readonly=true (fail-safe for free naming).
//  2. If behaviorExplicitReadonly is non-nil, that value wins unconditionally.
//  3. Compat exception: canonical "executor" without an explicit readonly
//     setting gets readonly=false to preserve the pre-A2 behaviour.
//     A deprecation warning is emitted at project load time (see
//     emitCanonicalBehaviorDeprecation in spec_loader.go); nothing is logged here.
//
// meta may be nil; its only effect is setting res.Worktree.
func applyCanonicalBehaviorOverrides(res *BehaviorResolution, meta *ProjectMeta, behaviorExplicitReadonly *bool) {
	res.Readonly = true // fail-safe default
	if behaviorExplicitReadonly != nil {
		res.Readonly = *behaviorExplicitReadonly
	} else if res.BehaviorName == "executor" {
		// Compat: canonical "executor" without explicit readonly → keep false.
		res.Readonly = false
	}
	if meta != nil {
		res.Worktree = meta.Worktree
	}
}

package orchestrator

import (
	"encoding/json"
	"fmt"
	"log/slog"
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

// DefaultBehavior is the reserved behavior name used when a CreateTaskRequest
// omits both behavior and behavior_spec. Projects are expected to define a
// behavior with this name in project.yaml's task_behaviors (typically with
// readonly: true) so that bare-task creation routes to a planning/triage step.
//
// Note: this is the canonical name; project.yaml files written with the
// legacy alias "plan" continue to work because spec_loader normalizes them
// to "supervisor" at load time (see BehaviorAliases).
const DefaultBehavior = "supervisor"

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
// When both behavior and behavior_spec are empty, the request is routed to DefaultBehavior.
func ResolveBehavior(meta *ProjectMeta, req BehaviorResolveRequest) (*BehaviorResolution, error) {
	if req.Behavior != "" && req.BehaviorSpec != nil {
		return nil, fmt.Errorf("behavior and behavior_spec are mutually exclusive")
	}
	if req.Behavior == "" && req.BehaviorSpec == nil {
		req.Behavior = DefaultBehavior
	}

	res := &BehaviorResolution{Payload: req.Payload}

	if req.BehaviorSpec != nil {
		spec := req.BehaviorSpec
		if spec.Name == "" {
			return nil, fmt.Errorf("behavior_spec.name is required")
		}
		res.BehaviorName = spec.Name
		res.Traits = spec.Traits
		// Phase 3-1: behavior-level readonly/worktree/branch_prefix/base_branch
		// and default_payload are gone. Inline specs receive the canonical
		// readonly/worktree treatment along with named behaviors below — set
		// here from project-top defaults (worktree only) and finalised by
		// applyCanonicalBehaviorOverrides.
		if meta != nil {
			res.Worktree = meta.Worktree
			res.BaseBranch = meta.BaseBranch
		}
		applyCanonicalBehaviorOverrides(res, meta)
		mergedInstructions, err := MergeDefaultInstructions(spec.DefaultInstruction, req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions merge: %w", err)
		}
		res.Instructions = mergedInstructions
		return res, nil
	}

	// Named behavior path (existing logic).
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
		// Phase 3-1: behavior-level readonly / worktree / branch_prefix /
		// base_branch / default_payload are deleted. readonly comes from the
		// behavior name (supervisor / executor) via
		// applyCanonicalBehaviorOverrides; worktree and base_branch come
		// from project-top fields.
		res.Worktree = meta.Worktree
		res.BaseBranch = meta.BaseBranch
		mergedInstructions, err := MergeDefaultInstructions(behavior.DefaultInstruction, req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions merge: %w", err)
		}
		res.Instructions = mergedInstructions

		applyCanonicalBehaviorOverrides(res, meta)
	} else if len(req.Instructions) > 0 {
		mergedInstructions, err := MergeDefaultInstructions(nil, req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions merge: %w", err)
		}
		res.Instructions = mergedInstructions
	}
	return res, nil
}

// applyCanonicalBehaviorOverrides enforces the Phase 3-1 readonly/worktree
// rules. After the behavior-level fields were removed, readonly is decided
// entirely by the canonical behavior name (supervisor=true, executor=false);
// non-canonical behaviors get readonly=false (the legacy zero-value default).
// worktree is taken from the project-top setting verbatim.
//
// meta may be nil when behavior_spec is in use without a project meta; the
// only effect of nil is that res.Worktree stays at its caller-supplied value
// (typically the bool zero) which mirrors the pre-Phase-3-1 behavior of
// inline specs.
func applyCanonicalBehaviorOverrides(res *BehaviorResolution, meta *ProjectMeta) {
	switch res.BehaviorName {
	case "supervisor":
		res.Readonly = true
	case "executor":
		res.Readonly = false
	default:
		res.Readonly = false
	}
	if meta != nil {
		res.Worktree = meta.Worktree
	}
}

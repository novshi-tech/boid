package orchestrator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// BehaviorResolution holds the resolved behavior fields after processing either
// a named behavior or an inline behavior_spec.
//
// The Worktree flag was removed in the branch-policy-simplification Phase 2
// (docs/plans/branch-policy-simplification.md). Post-cutover every project-visible
// job runs in a fresh sandbox clone, so per-task worktree isolation is no longer
// a concept the resolver needs to carry.
type BehaviorResolution struct {
	BehaviorName string
	Traits       []string
	Readonly     bool
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

// LookupBehavior finds a TaskBehavior in meta.TaskBehaviors by name. The name
// is matched verbatim — there is no alias or canonicalization step, so what a
// project.yaml (or workspace default) writes is exactly what callers must ask
// for.
//
// A nil meta reports not-found rather than panicking: ResolveBehavior's
// nil-meta paths and DefaultBehaviorResolvable both rely on that.
func LookupBehavior(meta *ProjectMeta, name string) (TaskBehavior, bool) {
	if meta == nil {
		return TaskBehavior{}, false
	}
	b, ok := meta.TaskBehaviors[name]
	return b, ok
}

// DefaultBehaviorResolvable reports whether ResolveBehavior would succeed for
// a task creation request that specifies neither behavior nor behavior_spec,
// given meta — mirroring the exact resolution order ResolveBehavior's own
// default-resolution branch below uses (docs/plans/
// workspace-default-project.md 論点d, fable 2巡目 m2): meta.DefaultTaskBehavior
// must be both set AND actually resolve via LookupBehavior, else a
// behavior literally named "supervisor" must exist in meta.TaskBehaviors,
// else it is not resolvable.
//
// Used at project-registration time (CreateProjectFromGitURL's
// project.yaml-less path) to decide whether the workspace default alone is
// sufficient before allowing registration to succeed. Deliberately NOT "does
// a default project definition exist" — a workspace default that defines
// task_behaviors but sets neither default_task_behavior nor a "supervisor"
// entry must still be rejected here, exactly as a behavior-unspecified task
// creation against it would 400 later.
func DefaultBehaviorResolvable(meta *ProjectMeta) bool {
	if meta == nil {
		return false
	}
	if meta.DefaultTaskBehavior != "" {
		_, ok := LookupBehavior(meta, meta.DefaultTaskBehavior)
		return ok
	}
	_, ok := meta.TaskBehaviors["supervisor"]
	return ok
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
		behavior, ok := LookupBehavior(meta, req.Behavior)
		if !ok {
			return nil, fmt.Errorf("behavior %q not found", req.Behavior)
		}
		res.Traits = behavior.Traits
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
// The meta parameter is retained for signature stability but no longer read
// (worktree resolution was removed in branch-policy-simplification Phase 2).
func applyCanonicalBehaviorOverrides(res *BehaviorResolution, _ *ProjectMeta, behaviorExplicitReadonly *bool) {
	res.Readonly = true // fail-safe default
	if behaviorExplicitReadonly != nil {
		res.Readonly = *behaviorExplicitReadonly
	} else if res.BehaviorName == "executor" {
		// Compat: canonical "executor" without explicit readonly → keep false.
		res.Readonly = false
	}
}

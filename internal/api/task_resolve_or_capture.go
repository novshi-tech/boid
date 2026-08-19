package api

// docs/plans/ingestion-identity.md PR-2 (B-2): 宛先解決と `captured` 着地。
//
// BoidOpTaskResolveOrCapture (internal/server/boid_executor.go) が呼ぶ唯一の
// 実装。「未着キー→新規 captured task」の判定 (I-4) と、その task へ identity
// を link する処理を **同一トランザクション**で行う — 作ったが link されて
// いない task が残ると、次の巡で同じキーがもう1枚作られてしまうため
// (PR-2 節)。
//
// 既存の action_send (ApplyAction) とは意図的に別 op のまま — 「解決と記録を
// 1 op に混ぜない」(PR-2 節)。created:false のとき、workspace は続けて
// action_send で attrs_set を押す。
//
// TaskWorkflowService に置く理由: Tx (Transactor) と Meta (behavior 解決に
// 要る) を既に持っているのがこのサービスだけであり、TaskAppService.CreateTask
// のような api.TaskStore 経由の非トランザクショナルな経路では
// create+link を1トランザクションに収められない (TaskStore.CreateTask は
// 呼び出し元が個々に持つ *sql.DB を internal に隠しているため)。

import (
	"context"
	"errors"
	"fmt"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ResolveOrCaptureRequest is BoidOpTaskResolveOrCapture's input: project +
// identity (always, I-2/I-3) plus title/description used ONLY when no task
// is currently bound to Identity (a fresh `captured` task is created).
type ResolveOrCaptureRequest struct {
	ProjectID   string
	Identity    string
	Title       string
	Description string
}

// ResolveOrCaptureResult is the op's return contract (PR-2 節 "B-2: push op
// は解決結果を返す"): the resolved task's id, and whether it was freshly
// created by this call (true) or already existed (false — the caller
// should follow up with action_send to record attrs_set, per the doc's
// "解決と記録を 1 op に混ぜない").
type ResolveOrCaptureResult struct {
	TaskID  string
	Created bool
}

// ResolveOrCapture resolves Identity (scoped to ProjectID, I-3) to an
// existing task, or — when unresolved — atomically creates a new `captured`
// triage task (I-4, J-9: daemon never advances it past captured) and links
// Identity to it.
//
// Invariants pinned by task_resolve_or_capture_test.go:
//   - resolve-then-create-and-link happens inside a single
//     Transactor.WithinTx call, so a create that isn't followed by a
//     successful link never commits (no orphan captured task without an
//     identity binding)
//   - a genuine identity conflict (LinkIdentity finding Identity already
//     bound to a DIFFERENT task) surfaces orchestrator.ErrIdentityConflict
//     UNWRAPPED so callers can errors.Is() it — same convention as PR-1's
//     api/task_identity.go methods
//   - description is subject to the same size cap as every other content
//     entry point (orchestrator.ValidateContentSize, J-10/A-5)
func (s *TaskWorkflowService) ResolveOrCapture(ctx context.Context, req ResolveOrCaptureRequest) (*ResolveOrCaptureResult, error) {
	if req.Identity == "" {
		return nil, fmt.Errorf("resolve or capture: identity must not be empty")
	}
	if req.ProjectID == "" {
		return nil, fmt.Errorf("resolve or capture: project id must not be empty")
	}
	// docs/plans/ingestion-identity.md PR-2 (B-2), J-10/A-5: one of the 4
	// mandatory entry points (task_create / task_update / action_send /
	// BoidOpTaskResolveOrCapture) — see orchestrator.ValidateContentSize's
	// own doc comment for the limit's value and how it was measured.
	if err := orchestrator.ValidateContentSize("description", []byte(req.Description)); err != nil {
		return nil, err
	}
	if s.Tx == nil {
		return nil, fmt.Errorf("resolve or capture: transactor unavailable")
	}

	// Behavior resolution is pure (in-memory project.yaml lookup, no DB) so
	// it's fine to run it up front — same 2-step hydrate-then-fallback
	// TaskAppService.CreateTask uses, so a workspace-level default project
	// definition's task_behaviors are visible here too.
	var meta *orchestrator.ProjectMeta
	if s.Meta != nil {
		if hydrated, err := s.Meta.GetWithWorkspace(ctx, req.ProjectID); err == nil && hydrated != nil {
			meta = hydrated
		} else if m, ok := s.Meta.Get(req.ProjectID); ok {
			meta = m
		}
	}

	var result ResolveOrCaptureResult
	txErr := s.Tx.WithinTx(func(tx TxStore) error {
		if existing, rerr := tx.ResolveIdentity(req.ProjectID, req.Identity); rerr == nil {
			result = ResolveOrCaptureResult{TaskID: existing.ID, Created: false}
			return nil
		} else if !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
			return fmt.Errorf("resolve or capture: resolve identity: %w", rerr)
		}

		// Unresolved: build a new captured triage task from the project's
		// resolved default behavior. Deliberately does NOT replicate
		// CreateTask's `${current_branch}` auto-expand fallback for an empty
		// BaseBranch (task_create.go) — every real ingestion caller today
		// (khi's daemon_sync.py ensure_task) already creates captured/
		// triaged tasks with no base_branch override and relies solely on
		// meta.BaseBranch (ResolveBehavior always sets res.BaseBranch =
		// meta.BaseBranch when meta != nil), so that fallback path has never
		// actually been exercised by ingestion. Skipping it keeps this
		// atomic block to pure, cheap, in-memory logic — no project workdir
		// filesystem access inside the transaction.
		res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{})
		if err != nil {
			return fmt.Errorf("resolve or capture: resolve behavior: %w", err)
		}

		task := &orchestrator.Task{
			ProjectID:    req.ProjectID,
			Title:        req.Title,
			Description:  req.Description,
			Status:       orchestrator.TaskStatusCaptured,
			Behavior:     res.BehaviorName,
			Traits:       res.Traits,
			Readonly:     res.Readonly,
			BranchPrefix: res.BranchPrefix,
			BaseBranch:   res.BaseBranch,
			Payload:      res.Payload,
			Instructions: res.Instructions,
		}
		if err := tx.CreateTask(task); err != nil {
			return fmt.Errorf("resolve or capture: create task: %w", err)
		}
		// task_triage sidecar row: makes the freshly captured task
		// discoverable as a triage task from birth — same reasoning as
		// CreateTask's own SeedTaskTriage call (task_create.go), just done
		// unconditionally and INSIDE the transaction here rather than
		// best-effort afterward, since we still hold transactional control
		// (CreateTask's own post-commit best-effort tolerance exists only
		// because by that point the task is already committed and can't be
		// rolled back — not the case here).
		if err := tx.SeedTaskTriage(task.ID); err != nil {
			return fmt.Errorf("resolve or capture: seed task_triage: %w", err)
		}
		if err := tx.LinkIdentity(req.ProjectID, req.Identity, task.ID); err != nil {
			// Deliberately unwrapped (not %w'd into a StatusError) so
			// errors.Is(err, orchestrator.ErrIdentityConflict) survives to
			// boid_executor.go, matching api/task_identity.go's own
			// convention. Returning this error rolls back the whole
			// WithinTx closure — the task/task_triage rows created above
			// never commit (I-4's "作ったが link されていない task が残る と
			// 次の巡で同じキーがもう1枚作る" is what this rollback prevents).
			return err
		}
		result = ResolveOrCaptureResult{TaskID: task.ID, Created: true}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return &result, nil
}

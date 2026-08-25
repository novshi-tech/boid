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
// existing task, or — when unresolved — atomically creates a new `parked`
// card (I-4, J-9: daemon never advances a card past capture — card machine
// v2, docs/plans/suggestion-as-state-transition-impl.md §3.5, folds the old
// `captured` status into `parked` directly) and links Identity to it.
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

	var result ResolveOrCaptureResult
	txErr := s.Tx.WithinTx(func(tx TxStore) error {
		if existing, rerr := tx.ResolveIdentity(req.ProjectID, req.Identity); rerr == nil {
			result = ResolveOrCaptureResult{TaskID: existing.ID, Created: false}
			return nil
		} else if !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
			return fmt.Errorf("resolve or capture: resolve identity: %w", rerr)
		}

		// Unresolved: build a fresh parked card. card-model-cleanup PR-2
		// (docs/plans/card-model-cleanup.md §3.7) purifies this path further
		// than before: it no longer calls ResolveBehavior AT ALL (not even
		// the earlier "call it but discard most of the result" shape) — a
		// card structurally has no ExecAttrs to put a resolved
		// behavior/traits/readonly/branch_prefix/base_branch/payload/
		// instructions into. This retires the base_branch-template landmine
		// that used to live here in full: a captured/parked task's
		// BaseBranch column doesn't exist anymore, so there is no dead,
		// unexpanded template string it could ever hold (see git history on
		// this file for the landmine's original, now-obsolete writeup —
		// design doc §7's "base_branch 未展開テンプレ地雷" row).
		task := &orchestrator.Task{
			ProjectID:   req.ProjectID,
			Title:       req.Title,
			Description: req.Description,
			Status:      orchestrator.TaskStatusParked,
			Type:        orchestrator.TaskTypeCard,
			Card:        &orchestrator.CardAttrs{},
		}
		if err := tx.CreateTask(task); err != nil {
			return fmt.Errorf("resolve or capture: create task: %w", err)
		}
		// SeedTaskTriage is GONE as of card-model-cleanup PR-2 (design doc
		// §3.6): the task above is already type='card' from the CreateTask
		// call itself, so there is no separate "seed the sidecar" step left
		// to perform — a card cannot be born rowless anymore.
		if err := tx.LinkIdentity(req.ProjectID, req.Identity, task.ID); err != nil {
			// Deliberately unwrapped (not %w'd into a StatusError) so
			// errors.Is(err, orchestrator.ErrIdentityConflict) survives to
			// boid_executor.go, matching api/task_identity.go's own
			// convention. Returning this error rolls back the whole
			// WithinTx closure — the task row created above never commits
			// (I-4's "作ったが link されていない task が残る と 次の巡で同じ
			// キーがもう1枚作る" is what this rollback prevents).
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

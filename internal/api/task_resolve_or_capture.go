package api

// 宛先解決と `captured` 着地。BoidOpTaskResolveOrCapture
// (internal/server/boid_executor.go) が呼ぶ唯一の実装。「未着キー→新規 captured
// task」の判定と、その task へ identity を link する処理を**同一トランザクション**
// で行う — 作ったが link されていない task が残ると、次の巡で同じキーがもう1枚
// 作られてしまうため。
//
// 既存の action_send (ApplyAction) とは意図的に別 op のまま — 解決と記録を 1 op
// に混ぜない。created:false のとき、workspace は続けて action_send で attrs_set
// を押す。
//
// TaskWorkflowService に置く理由: Tx (Transactor) と Meta (behavior 解決に
// 要る) を既に持っているのがこのサービスだけであり、TaskAppService.CreateTask
// のような api.TaskStore 経由の非トランザクショナルな経路では
// create+link を1トランザクションに収められない。

import (
	"context"
	"errors"
	"fmt"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ResolveOrCaptureRequest = apiwire.ResolveOrCaptureRequest
type ResolveOrCaptureResult = apiwire.ResolveOrCaptureResult

// ResolveOrCapture resolves Identity (scoped to ProjectID) to an existing
// task, or — when unresolved — atomically creates a new `parked` card
// (the daemon never advances a card past capture) and links Identity to it.
//
// Invariants pinned by task_resolve_or_capture_test.go:
//   - resolve-then-create-and-link happens inside a single
//     Transactor.WithinTx call, so a create that isn't followed by a
//     successful link never commits (no orphan captured task without an
//     identity binding)
//   - a genuine identity conflict (LinkIdentity finding Identity already
//     bound to a DIFFERENT task) surfaces orchestrator.ErrIdentityConflict
//     UNWRAPPED so callers can errors.Is() it — same convention as
//     api/task_identity.go's methods
//   - description is subject to the same size cap as every other content
//     entry point (orchestrator.ValidateContentSize)
func (s *TaskWorkflowService) ResolveOrCapture(ctx context.Context, req ResolveOrCaptureRequest) (*ResolveOrCaptureResult, error) {
	if req.Identity == "" {
		return nil, fmt.Errorf("resolve or capture: identity must not be empty")
	}
	scope := orchestrator.IdentityScope{ProjectID: req.ProjectID, WorkspaceID: req.WorkspaceID}
	if req.ProjectID == "" {
		if req.WorkspaceID == "" {
			return nil, fmt.Errorf("resolve or capture: workspace id must not be empty")
		}
		if err := orchestrator.ValidWorkspaceSlug(req.WorkspaceID); err != nil {
			return nil, err
		}
	}
	if req.URL != nil {
		if err := orchestrator.ValidateIdentityURL(*req.URL); err != nil {
			return nil, err
		}
	}
	if err := orchestrator.ValidateContentSize("description", []byte(req.Description)); err != nil {
		return nil, err
	}
	if s.Tx == nil {
		return nil, fmt.Errorf("resolve or capture: transactor unavailable")
	}

	var result ResolveOrCaptureResult
	txErr := s.Tx.WithinTx(func(tx TxStore) error {
		writer := scopedIdentityWriter{tx, scope}
		var metadata identityMetadataWriter
		if req.URL != nil || req.DisplayName != nil {
			var ok bool
			if scope.ProjectID == "" {
				_, ok = tx.(workspaceIdentityWriter)
			} else {
				_, ok = tx.(identityMetadataWriter)
			}
			metadata = writer
			if !ok {
				return errIdentityMetadataUnavailable
			}
		}
		if existing, rerr := writer.ResolveIdentity(req.ProjectID, req.Identity); rerr == nil {
			if metadata != nil {
				if err := metadata.UpdateIdentityMetadata(req.ProjectID, req.Identity, existing.ID, req.URL, req.DisplayName); err != nil {
					return err
				}
			}
			result = ResolveOrCaptureResult{TaskID: existing.ID, Created: false}
			return nil
		} else if !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
			return fmt.Errorf("resolve or capture: resolve identity: %w", rerr)
		}

		// Unresolved: build a fresh parked card. This never calls
		// ResolveBehavior at all — a card structurally has no ExecAttrs to
		// put a resolved behavior/traits/readonly/branch_prefix/base_branch/
		// payload/instructions into.
		task := &orchestrator.Task{
			ProjectID:   req.ProjectID,
			WorkspaceID: req.WorkspaceID,
			Title:       req.Title,
			Description: req.Description,
			Status:      orchestrator.TaskStatusParked,
			Type:        orchestrator.TaskTypeCard,
			Card:        &orchestrator.CardAttrs{},
		}
		if err := tx.CreateTask(task); err != nil {
			return fmt.Errorf("resolve or capture: create task: %w", err)
		}
		// The task above is already type='card' from the CreateTask call
		// itself, so there is no separate "seed the sidecar" step left to
		// perform — a card cannot be born rowless.
		if err := writer.LinkIdentity(req.ProjectID, req.Identity, task.ID); err != nil {
			// Deliberately unwrapped (not %w'd into a StatusError) so
			// errors.Is(err, orchestrator.ErrIdentityConflict) survives to
			// boid_executor.go, matching api/task_identity.go's own
			// convention. Returning this error rolls back the whole
			// WithinTx closure — the task row created above never commits,
			// preventing an orphan captured task that would let the same
			// key create a second one next cycle.
			return err
		}
		if metadata != nil {
			if err := metadata.UpdateIdentityMetadata(req.ProjectID, req.Identity, task.ID, req.URL, req.DisplayName); err != nil {
				return err
			}
		}
		// The card's own creation record.
		if err := tx.CreateAction(ctx, &orchestrator.Action{
			TaskID: task.ID,
			Type:   orchestrator.ActionTypeCardCreated,
			Actor:  updateActor(ctx),
		}); err != nil {
			return fmt.Errorf("resolve or capture: record creation: %w", err)
		}
		result = ResolveOrCaptureResult{TaskID: task.ID, Created: true}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return &result, nil
}

package api

// docs/plans/ingestion-identity.md PR-2 (B-2): 宛先解決と着地 status の選択。
//
// BoidOpTaskResolveOrCapture (internal/server/boid_executor.go) が呼ぶ唯一の
// 実装。「未着キー→新規 triage task」の判定 (I-4) と、その task へ identity
// を link する処理を **同一トランザクション**で行う — 作ったが link されて
// いない task が残ると、次の巡で同じキーがもう1枚作られてしまうため
// (PR-2 節)。
//
// 着地 status は既定 `captured`（従来どおり、後方互換）だが、khi-task-collector
// rebuild.md §11/§5.6 を受けて `triaged` も選べる（ingestion-identity.md の
// J-9「daemon が triaged まで進めることはない」の部分撤回 — 同 doc PR-2 節の
// 訂正セクションに「いつ・なぜ」の記録がある）。既存 CreateTask
// (task_create.go: allowedCreateInitialStatuses) と同じ「許可語彙は
// map で明示、許可外は明示的エラー」パターンを踏襲する。ready/working/pending
// のような「進めてよい」を含意する状態は意図的に許可しない — workspace 側が
// state を押し返す経路を開けない (I-4 の「daemon に届く時点で既に起票に値する
// と判断済み」という前提の外に出る要求は拒否する)。
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
// identity (always, I-2/I-3) plus title/description/status used ONLY when no
// task is currently bound to Identity (a fresh task is created).
//
// Status chooses the landing status for that fresh task: "" (unset) and
// "captured" both mean the pre-change default; "triaged" is the opt-in
// added for callers (khi) that already screen everything they push, so
// landing in `captured` would just mean an extra no-op transition before
// reaching the state the caller already determined. See
// resolveLandingStatus for the full allowlist and rejection behavior.
type ResolveOrCaptureRequest struct {
	ProjectID   string
	Identity    string
	Title       string
	Description string
	Status      string
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

// allowedResolveOrCaptureStatuses is the landing-status allowlist for a
// freshly created task (docs/plans/khi-task-collector rebuild.md §11,
// partial retraction of ingestion-identity.md J-9 — see that doc's PR-2 節
// 訂正セクション for the "いつ・なぜ・どこまで" record). Deliberately NOT
// task_create.go's allowedCreateInitialStatuses:
//   - "pending" is excluded — resolve-or-capture only ever creates
//     ingestion-owned pre-execution tasks, never the pending→executing
//     track; there is no auto_start-equivalent concept here
//   - every status at or past `ready` (ready/working/executing/awaiting/
//     done/aborted/dropped/parked/...) is excluded — accepting one would
//     let a workspace caller push a task straight into a state that
//     implies "go", reopening the exact state-pushback hole flagged by
//     rebuild.md §3.2 and khi's own daemon_sync.py:161-165 incident ("Web
//     UI 操作が次巡で巻き戻る")
//
// "" (unset) keeps the pre-change behavior (captured), so every existing
// caller (khi's daemon_sync.py ensure_task, as of this change) is
// unaffected by this being additive.
var allowedResolveOrCaptureStatuses = map[string]orchestrator.TaskStatus{
	"":         orchestrator.TaskStatusCaptured,
	"captured": orchestrator.TaskStatusCaptured,
	"triaged":  orchestrator.TaskStatusTriaged,
}

// resolveLandingStatus validates req.Status against
// allowedResolveOrCaptureStatuses and returns the orchestrator.TaskStatus a
// freshly created task should land in.
//
// This is the ONLY place the check runs — deliberately in the api layer
// (TaskWorkflowService.ResolveOrCapture), not the sandbox shim/broker/
// executor (internal/sandbox/boid_shim.go, broker.go,
// internal/server/boid_executor.go all just forward the raw string through
// unchecked) — so the vocabulary lockdown stays authoritative regardless of
// entry point, rather than being a shim-only check a future caller could
// route around.
func resolveLandingStatus(status string) (orchestrator.TaskStatus, error) {
	resolved, ok := allowedResolveOrCaptureStatuses[status]
	if !ok {
		return "", fmt.Errorf("resolve or capture: status: unknown value %q (allowed: captured, triaged)", status)
	}
	return resolved, nil
}

// ResolveOrCapture resolves Identity (scoped to ProjectID, I-3) to an
// existing task, or — when unresolved — atomically creates a new triage
// task and links Identity to it. The new task's landing status is
// req.Status (default `captured`, opt-in `triaged` — resolveLandingStatus;
// I-4, J-9 partial retraction).
//
// Invariants pinned by task_resolve_or_capture_test.go:
//   - resolve-then-create-and-link happens inside a single
//     Transactor.WithinTx call, so a create that isn't followed by a
//     successful link never commits (no orphan task without an identity
//     binding, regardless of which status it would have landed in)
//   - a genuine identity conflict (LinkIdentity finding Identity already
//     bound to a DIFFERENT task) surfaces orchestrator.ErrIdentityConflict
//     UNWRAPPED so callers can errors.Is() it — same convention as PR-1's
//     api/task_identity.go methods
//   - description is subject to the same size cap as every other content
//     entry point (orchestrator.ValidateContentSize, J-10/A-5)
//   - req.Status is validated unconditionally, before any transaction opens
//     — even on the resolve-only path where it would otherwise be inert
//     (identity already resolves, so no task is created either way). A
//     caller's bad status value fails the same way regardless of unrelated
//     data state (deliberate judgment call — see this package's test
//     TestResolveOrCapture_InvalidStatus_AlreadyResolved_StillRejects)
func (s *TaskWorkflowService) ResolveOrCapture(ctx context.Context, req ResolveOrCaptureRequest) (*ResolveOrCaptureResult, error) {
	if req.Identity == "" {
		return nil, fmt.Errorf("resolve or capture: identity must not be empty")
	}
	if req.ProjectID == "" {
		return nil, fmt.Errorf("resolve or capture: project id must not be empty")
	}
	landingStatus, err := resolveLandingStatus(req.Status)
	if err != nil {
		return nil, err
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

		// Unresolved: build a new triage task (landingStatus — captured by
		// default, triaged if req.Status requested it) from the project's
		// resolved default behavior. Deliberately does NOT replicate EITHER
		// of CreateTask's (task_create.go) two base_branch template-expand
		// steps — and, unlike that earlier version of this comment, both
		// branches need spelling out because both are skipped:
		//
		//   - meta.BaseBranch == "": CreateTask auto-expands `${current_branch}`
		//     as a fallback. Every real ingestion caller today (khi's
		//     daemon_sync.py ensure_task) already creates captured/triaged
		//     tasks with no base_branch override and relies solely on
		//     meta.BaseBranch (ResolveBehavior always sets res.BaseBranch =
		//     meta.BaseBranch when meta != nil), so that fallback path has
		//     never actually been exercised by ingestion.
		//   - meta.BaseBranch != "" (e.g. a template like
		//     "feature/${TASK_REMOTE_ID}"): CreateTask runs it through
		//     ExpandTaskBaseBranch (${TASK_REMOTE_ID}) then ExpandBaseBranch
		//     (${current_branch}). Neither runs here. ExpandTaskBaseBranch
		//     can't be called meaningfully: this request never carries a
		//     RemoteID (a freshly captured task doesn't have one until
		//     triage assigns it), and ExpandTaskBaseBranch hard-errors when
		//     a template references ${TASK_REMOTE_ID} but remoteID is empty
		//     — calling it unconditionally would turn every capture for such
		//     a project into a failure, which is worse than the status quo.
		//     ExpandBaseBranch is skipped too: it shells out to git against
		//     the project workdir, and this whole block deliberately stays
		//     pure/in-memory with no project workdir filesystem access —
		//     db.go's SetMaxOpenConns(1) means a git subprocess call here
		//     would hold the daemon's single DB connection hostage for the
		//     duration of the shell-out.
		//
		// The consequence (stated plainly, not just implied): when
		// meta.BaseBranch is a template, the LITERAL, UNEXPANDED template
		// string is what lands in a freshly captured task's
		// tasks.base_branch column — e.g. "feature/${TASK_REMOTE_ID}"
		// verbatim, never resolved, never validated, even when RemoteID
		// would already be available.
		//
		// This is acceptable only because a captured/triaged task's own
		// BaseBranch is dead data while it stays in ingestion-owned
		// pre-execution status: machine.go's transition table has no rule
		// from captured/triaged/parked/ready/working to executing — the
		// ONLY paths to executing are pending→(start) and
		// done/aborted→(reopen) — so nothing ever reads this literal
		// template to build a clone. If a future change lets a triage task
		// reach executing directly (bypassing pending), this landmine
		// becomes reachable and must be revisited then — either expand here
		// (accepting the git-shell-out-in-tx cost) or validate/reject a
		// templated meta.BaseBranch before it ever reaches this path.
		//
		// This premise is unaffected by req.Status choosing `triaged`
		// instead of `captured` as the landing status (J-9 partial
		// retraction, see resolveLandingStatus): the "no rule to executing"
		// argument above was already stated in terms of the WHOLE
		// captured/triaged/parked/ready/working group, not specifically
		// about how a task GOT to `triaged` (via CreateTask's own
		// initial_status=triaged, via the "triage" manual action, or now
		// directly via this op) — machine.go treats a `triaged` task
		// identically regardless of its path there. Re-verified against
		// machine.go's rule table while adding req.Status: still no rule
		// transitions anything TO "pending" either, so a triage-track task
		// can never even reach the one status that DOES have a path to
		// executing.
		res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{})
		if err != nil {
			return fmt.Errorf("resolve or capture: resolve behavior: %w", err)
		}

		task := &orchestrator.Task{
			ProjectID:    req.ProjectID,
			Title:        req.Title,
			Description:  req.Description,
			Status:       landingStatus,
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
		// task_triage sidecar row: makes the freshly created task
		// discoverable as a triage task from birth, regardless of whether
		// it landed captured or triaged — same reasoning as
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

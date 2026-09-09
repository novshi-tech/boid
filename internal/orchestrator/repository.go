package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

type TaskRepository struct {
	db db.DBTX
	// cardEventResolver backs CreateAction's card-event ingest decision. nil
	// disables that ingest step entirely — CreateAction still writes the
	// action row as before. Set via SetCardEventResolver post-construction
	// rather than a NewTaskRepository parameter, since most call sites never
	// use it.
	cardEventResolver CardEventResolver
}

func NewTaskRepository(db db.DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

// SetCardEventResolver wires the card_events.command lookup
// CreateAction's card-event ingest step needs — see the cardEventResolver
// field's own doc comment.
func (r *TaskRepository) SetCardEventResolver(resolver CardEventResolver) {
	r.cardEventResolver = resolver
}

func (r *TaskRepository) CreateTask(task *Task) error {
	return CreateTask(r.db, task)
}

func (r *TaskRepository) GetTask(id string) (*Task, error) {
	return GetTask(r.db, id)
}

// GetTaskStatus satisfies api.TaskStatusReader — the narrow read
// api.TaskAppService.WaitTaskTerminal polls with. See GetTaskStatus (store.go)
// for why it is not GetTask.
func (r *TaskRepository) GetTaskStatus(id string) (TaskStatus, error) {
	return GetTaskStatus(r.db, id)
}

func (r *TaskRepository) ListTasks(filter TaskFilter) ([]*Task, error) {
	return ListTasks(r.db, filter)
}

func (r *TaskRepository) UpdateTask(task *Task) error {
	return UpdateTask(r.db, task)
}

// TouchTaskUpdatedAt satisfies api.TaskUpdatedAtToucher.
func (r *TaskRepository) TouchTaskUpdatedAt(id string) error {
	return TouchTaskUpdatedAt(r.db, id)
}

func (r *TaskRepository) DeleteTask(id string) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return DeleteTask(r.db, id)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return DeleteTask(tx, id)
	})
}

func (r *TaskRepository) FindTaskByRemote(remoteID string) (*Task, error) {
	return FindTaskByRemote(r.db, remoteID)
}

func (r *TaskRepository) FindTaskByRef(ref, parentID, projectID string) (*Task, error) {
	return FindTaskByRef(r.db, ref, parentID, projectID)
}

func (r *TaskRepository) FindTaskByIdempotencyKey(projectID, parentID, idempotencyKey string) (*Task, error) {
	return FindTaskByIdempotencyKey(r.db, projectID, parentID, idempotencyKey)
}

func (r *TaskRepository) ListChildren(parentID string) ([]*Task, error) {
	return ListChildren(r.db, parentID)
}

// CreateAction persists action, then — within the SAME transaction — queues
// a card_events request for the target card when eligible. Same dual-mode
// shape as DeleteTask above: nests inside an already-open tx when r.db is
// one, or opens its own spanning transaction when r.db is a raw *sql.DB, so
// the INSERT and the ingest can never commit independently.
func (r *TaskRepository) CreateAction(ctx context.Context, action *Action) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return CreateAction(ctx, r.db, action, r.cardEventResolver)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return CreateAction(ctx, tx, action, r.cardEventResolver)
	})
}

func (r *TaskRepository) ListActionsByTask(taskID string) ([]*Action, error) {
	return ListActionsByTask(r.db, taskID)
}

// ListActionsSince is the workspace-scoped action_list read.
func (r *TaskRepository) ListActionsSince(filter ActionListFilter) ([]*Action, string, error) {
	return ListActionsSince(r.db, filter)
}

func (r *TaskRepository) UpsertTaskTriage(tt *CardAttrs) error {
	return UpsertTaskTriage(r.db, tt)
}

func (r *TaskRepository) GetTaskTriage(taskID string) (*CardAttrs, error) {
	return GetTaskTriage(r.db, taskID)
}

func (r *TaskRepository) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*CardAttrs, error) {
	return ListTaskTriageByTaskIDs(r.db, taskIDs)
}

func (r *TaskRepository) DeleteTaskTriage(taskID string) error {
	return DeleteTaskTriage(r.db, taskID)
}

func (r *TaskRepository) ParkedFrom(taskID string) (TaskStatus, error) {
	return ParkedFrom(r.db, taskID)
}

// LinkIdentity / UnlinkIdentity / UnlinkAllForTask / ResolveIdentity /
// ListIdentitiesByTask are thin wrappers over task_identity.go's identity index.
func (r *TaskRepository) LinkIdentity(projectID, identity, taskID string) error {
	return LinkIdentity(r.db, projectID, identity, taskID)
}

func (r *TaskRepository) UnlinkIdentity(projectID, identity string) error {
	return UnlinkIdentity(r.db, projectID, identity)
}

func (r *TaskRepository) UnlinkAllForTask(taskID string) error {
	return UnlinkAllForTask(r.db, taskID)
}

func (r *TaskRepository) ResolveIdentity(projectID, identity string) (*Task, error) {
	return ResolveIdentity(r.db, projectID, identity)
}

func (r *TaskRepository) ListIdentitiesByTask(taskID string) ([]string, error) {
	return ListIdentitiesByTask(r.db, taskID)
}
func (r *TaskRepository) UpdateIdentityMetadata(projectID, identity string, url, displayName *string) error {
	return UpdateIdentityMetadata(r.db, projectID, identity, url, displayName)
}
func (r *TaskRepository) ListIdentityMetadataByTask(taskID string) ([]TaskIdentity, error) {
	return ListIdentityMetadataByTask(r.db, taskID)
}

// CreateTriggerRun / CompleteTriggerRun / ListInFlightTriggerRuns /
// LatestTriggerRun are thin wrappers over trigger_run.go's trigger_runs ledger.
func (r *TaskRepository) CreateTriggerRun(run *TriggerRun) error {
	return CreateTriggerRun(r.db, run)
}

func (r *TaskRepository) CompleteTriggerRun(id string, finishedAt time.Time, exitCode int) error {
	return CompleteTriggerRun(r.db, id, finishedAt, exitCode)
}

func (r *TaskRepository) ListInFlightTriggerRuns() ([]*TriggerRun, error) {
	return ListInFlightTriggerRuns(r.db)
}

func (r *TaskRepository) LatestTriggerRun(projectID, triggerName string) (*TriggerRun, error) {
	return LatestTriggerRun(r.db, projectID, triggerName)
}

// SetTriggerRunJobID / DeleteTriggerRun back the insert-then-dispatch split
// — see trigger_run.go's own doc comments.
func (r *TaskRepository) SetTriggerRunJobID(id, jobID string) error {
	return SetTriggerRunJobID(r.db, id, jobID)
}

func (r *TaskRepository) DeleteTriggerRun(id string) error {
	return DeleteTriggerRun(r.db, id)
}

// IngestSignals / GetSignalCursor / ListSignals / ClaimSignals / AckSignals /
// HasPendingSignals wrap signal_store.go's inbox store.
//
// IngestSignals and ClaimSignals each need their SQL statements to run as
// one transaction, so — unlike every other wrapper on this type — they
// follow DeleteTask's pattern above rather than being one-line delegators:
// when r.db is a raw *sql.DB, they open their own spanning transaction.
func (r *TaskRepository) IngestSignals(workspaceID, service, connector string, rows []SignalIngestRow) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return IngestSignals(r.db, workspaceID, service, connector, rows)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return IngestSignals(tx, workspaceID, service, connector, rows)
	})
}

func (r *TaskRepository) GetSignalCursor(workspaceID, service, connector string) (string, error) {
	return GetSignalCursor(r.db, workspaceID, service, connector)
}

func (r *TaskRepository) ListSignals(filter SignalFilter) ([]*Signal, error) {
	return ListSignals(r.db, filter)
}

func (r *TaskRepository) ClaimSignals(workspaceID string, limit, maxAttempts int) ([]*Signal, error) {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return ClaimSignals(r.db, workspaceID, limit, maxAttempts)
	}
	var claimed []*Signal
	err := db.InTxDB(conn, func(tx db.DBTX) error {
		c, err := ClaimSignals(tx, workspaceID, limit, maxAttempts)
		if err != nil {
			return err
		}
		claimed = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (r *TaskRepository) ClaimSignalIDs(workspaceID string, ids []string) error {
	return ClaimSignalIDs(r.db, workspaceID, ids)
}

func (r *TaskRepository) AckSignals(workspaceID string, ids []string) error {
	return AckSignals(r.db, workspaceID, ids)
}

func (r *TaskRepository) HasPendingSignals(workspaceID string, maxAttempts int) (bool, error) {
	return HasPendingSignals(r.db, workspaceID, maxAttempts)
}

// GetCardRequest backs `boid card context`'s live lookup of the calling
// job's card_requests row (server.cardRequestReader).
func (r *TaskRepository) GetCardRequest(id string) (*CardRequest, error) {
	return GetCardRequest(r.db, id)
}

func (r *TaskRepository) GetCardRequestByTaskTarget(taskID string) (*CardRequest, error) {
	return GetCardRequestByTaskTarget(r.db, taskID)
}

// AttachCardRequestOwned backs `boid agent start`'s recording of the
// session continuation it just created (server.cardRequestReader) — the
// caller's job id is asserted in the write itself, not trusted from an
// earlier read (see AttachCardRequestOwned's own doc comment).
func (r *TaskRepository) AttachCardRequestOwned(id, expectedLauncherJobID, targetKind, targetID string) error {
	return AttachCardRequestOwned(r.db, id, expectedLauncherJobID, targetKind, targetID)
}

// ForceReleaseCardRequest backs POST /api/card-requests/{id}/release, the
// operator escape hatch for a stuck slot (api.CardRequestReleaseStore).
// FailCardRequest is two UPDATEs that must land together — same
// InTxDB-over-raw-*sql.DB shape as ClaimSignals above.
func (r *TaskRepository) ForceReleaseCardRequest(id, reason string) ([]ForceReleasedSibling, error) {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return ForceReleaseCardRequest(r.db, id, reason)
	}
	var siblings []ForceReleasedSibling
	err := db.InTxDB(conn, func(tx db.DBTX) error {
		var terr error
		siblings, terr = ForceReleaseCardRequest(tx, id, reason)
		return terr
	})
	return siblings, err
}

// CreateCardRequest backs a card command launcher's request creation — a
// single INSERT, no transaction needed (api.CardCommandLauncherStore).
func (r *TaskRepository) CreateCardRequest(req *CardRequest) error {
	return CreateCardRequest(r.db, req)
}

// FailCardRequest backs a card command launcher's slot release on dispatch
// failure — two UPDATEs (fail + release folded siblings) that must land
// together, same InTxDB-over-raw-*sql.DB shape as ForceReleaseCardRequest.
func (r *TaskRepository) FailCardRequest(id, errText string) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return FailCardRequest(r.db, id, errText)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return FailCardRequest(tx, id, errText)
	})
}

// CountActiveCardRequests backs the shared execution-slot occupancy check —
// a single read, no transaction needed.
func (r *TaskRepository) CountActiveCardRequests(cardID string) (int, error) {
	return CountActiveCardRequests(r.db, cardID)
}

// ListCardRequestsByCard backs a manual command's "return a link to the
// current execution" response when the card's slot is already occupied — a
// single read, no transaction needed.
func (r *TaskRepository) ListCardRequestsByCard(cardID string) ([]*CardRequest, error) {
	return ListCardRequestsByCard(r.db, cardID)
}

// ListActiveCardRequests backs `boid task diagnose-cards`' bulk lookup of
// every card with an active request — a single read, no transaction needed.
func (r *TaskRepository) ListActiveCardRequests() ([]*CardRequest, error) {
	return ListActiveCardRequests(r.db)
}

// ActiveCardRequestsByCardIDs backs the task list's per-row command activity
// badge — a single batched read across every card on the current page, no
// transaction needed.
func (r *TaskRepository) ActiveCardRequestsByCardIDs(cardIDs []string) (map[string]*CardRequest, error) {
	return ListActiveCardRequestsByCardIDs(r.db, cardIDs)
}

// TaskStatusesByIDs backs the task list's per-row work-child activity
// badge — a single batched read of a page's dispatched children's real task
// status, no transaction needed.
func (r *TaskRepository) TaskStatusesByIDs(taskIDs []string) (map[string]TaskStatus, error) {
	return TaskStatusesByIDs(r.db, taskIDs)
}

// ReleaseCardRequestForTerminalTargetWithCard backs finalizeTerminal's
// immediate slot release AND the immediate re-dispatch attempt that follows
// it — same InTxDB-over-raw-*sql.DB shape as FailCardRequest.
func (r *TaskRepository) ReleaseCardRequestForTerminalTargetWithCard(targetKind, targetID string, success bool) (found bool, cardID string, err error) {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return ReleaseCardRequestForTerminalTargetWithCard(r.db, targetKind, targetID, success)
	}
	err = db.InTxDB(conn, func(tx db.DBTX) error {
		f, cid, ferr := ReleaseCardRequestForTerminalTargetWithCard(tx, targetKind, targetID, success)
		found, cardID = f, cid
		return ferr
	})
	return found, cardID, err
}

// ClaimQueuedCardRequestsForDispatch backs the automatic card-request
// dispatcher's claim step, same InTxDB-over-raw-*sql.DB shape as
// ForceReleaseCardRequest. A "nothing claimed" sentinel
// (IsCardRequestDispatchSkip) is captured separately rather than returned
// as the closure's own error, so a drain side effect it may carry still
// commits instead of being rolled back with it.
func (r *TaskRepository) ClaimQueuedCardRequestsForDispatch(cardID, launcherJobID, expectedCommandKey string, def CardRequestDefinition) (*CardRequest, []*CardRequest, error) {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return ClaimQueuedCardRequestsForDispatch(r.db, cardID, launcherJobID, expectedCommandKey, def)
	}
	var primary *CardRequest
	var folded []*CardRequest
	var skip error
	err := db.InTxDB(conn, func(tx db.DBTX) error {
		p, f, cerr := ClaimQueuedCardRequestsForDispatch(tx, cardID, launcherJobID, expectedCommandKey, def)
		primary, folded = p, f
		if IsCardRequestDispatchSkip(cerr) {
			skip = cerr
			return nil
		}
		return cerr
	})
	if err != nil {
		return nil, nil, err
	}
	return primary, folded, skip
}

// PeekOldestQueuedCardRequest backs the automatic dispatcher's pre-claim
// command definition resolution — a single read, no transaction needed.
func (r *TaskRepository) PeekOldestQueuedCardRequest(cardID string) (id, commandKey string, err error) {
	return PeekOldestQueuedCardRequest(r.db, cardID)
}

// ListCardIDsWithQueuedCardRequests backs the periodic dispatch sweep's work
// list — a single read, no transaction needed.
func (r *TaskRepository) ListCardIDsWithQueuedCardRequests() ([]string, error) {
	return ListCardIDsWithQueuedCardRequests(r.db)
}

// ClearCardForceReleaseBarrier backs a human-issued card command / Go /
// explicit retry ending automatic-dispatch suppression for a card — a
// single DELETE, no transaction needed on its own (callers that need it
// atomic with a claim run it inside their own WithinTx via TxStore instead).
func (r *TaskRepository) ClearCardForceReleaseBarrier(cardID string) error {
	return ClearCardForceReleaseBarrier(r.db, cardID)
}

// SetCardForceReleaseBarrier backs test setup / a future operator-facing
// force-release path that wants to plant a barrier directly — a single
// upsert, no transaction needed on its own.
func (r *TaskRepository) SetCardForceReleaseBarrier(cardID string) error {
	return SetCardForceReleaseBarrier(r.db, cardID)
}

// HasCardForceReleaseBarrier backs a caller checking a card's suppression
// state directly — a single read, no transaction needed.
func (r *TaskRepository) HasCardForceReleaseBarrier(cardID string) (bool, error) {
	return HasCardForceReleaseBarrier(r.db, cardID)
}

// CreateTaskLinkedToCardRequest runs CreateTask(t) and
// AttachCardRequestOwned(requestID, ownerJobID, "task", t.ID) IN THE SAME
// TRANSACTION, so a task continuation is never observable without its
// request association, AND the attach re-asserts ownerJobID as the current
// owner in its own write rather than trusting an earlier, separate
// ownership read (see AttachCardRequestOwned's own doc comment for the race
// this closes). A retry that hits CreateTask's own Ref/IdempotencyKey
// get-or-create and finds the SAME task already attached is treated as
// success, not an error.
func (r *TaskRepository) CreateTaskLinkedToCardRequest(t *Task, requestID, ownerJobID string) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return createTaskLinkedToCardRequest(r.db, t, requestID, ownerJobID)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return createTaskLinkedToCardRequest(tx, t, requestID, ownerJobID)
	})
}

// createTaskLinkedToCardRequest is CreateTaskLinkedToCardRequest's body,
// dbtx-parameterized so both the *sql.DB (wraps its own tx) and already-
// inside-a-tx (nested call, e.g. from a caller that already holds one)
// shapes share one implementation.
func createTaskLinkedToCardRequest(dbtx db.DBTX, t *Task, requestID, ownerJobID string) error {
	if err := CreateTask(dbtx, t); err != nil {
		return fmt.Errorf("create task linked to card request: %w", err)
	}
	if err := AttachCardRequestOwned(dbtx, requestID, ownerJobID, CardRequestTargetKindTask, t.ID); err != nil {
		if errors.Is(err, ErrCardRequestInvalidTransition) {
			// Idempotent retry (t.ID unchanged via Ref/IdempotencyKey
			// get-or-create) or a request that raced to a different
			// terminal state — re-read and decide rather than assume.
			existing, gerr := GetCardRequest(dbtx, requestID)
			if gerr != nil {
				return fmt.Errorf("create task linked to card request: re-read %q after attach conflict: %w", requestID, gerr)
			}
			if existing.TargetKind == CardRequestTargetKindTask && existing.TargetID == t.ID {
				// Already correctly attached to this exact task — the
				// retry converges, not an error.
				return nil
			}
			return fmt.Errorf("create task linked to card request: %q is already attached to %s %q, not task %q: %w",
				requestID, existing.TargetKind, existing.TargetID, t.ID, ErrCardRequestInvalidTransition)
		}
		return fmt.Errorf("create task linked to card request: attach: %w", err)
	}
	return nil
}

type ProjectRepository struct {
	db db.DBTX
}

func NewProjectRepository(db db.DBTX) *ProjectRepository {
	return &ProjectRepository{db: db}
}

func (r *ProjectRepository) CreateProject(project *Project) error {
	return CreateProject(r.db, project)
}

func (r *ProjectRepository) GetProject(id string) (*Project, error) {
	return GetProject(r.db, id)
}

func (r *ProjectRepository) ListProjects() ([]*Project, error) {
	return ListProjects(r.db)
}

func (r *ProjectRepository) SetProjectWorkspace(projectID, workspaceID string) error {
	return SetProjectWorkspace(r.db, projectID, workspaceID)
}

// AssignWorkspaceIfExists atomically checks-then-assigns. See the
// package-level function's doc comment. r.db must be a *sql.DB — every
// production wiring path constructs ProjectRepository with the daemon's
// single *sql.DB handle.
func (r *ProjectRepository) AssignWorkspaceIfExists(projectID, workspaceID string) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("AssignWorkspaceIfExists: repository is not backed by a *sql.DB (got %T)", r.db)
	}
	return AssignWorkspaceIfExists(conn, projectID, workspaceID)
}

func (r *ProjectRepository) ListWorkspaces() ([]*WorkspaceSummary, error) {
	return ListWorkspaces(r.db)
}

// ListProjectWorkspaceReferences returns project_workspaces membership
// directly (see the package-level function's doc comment for why this
// differs from ListWorkspaces and who still needs it).
func (r *ProjectRepository) ListProjectWorkspaceReferences() ([]*WorkspaceSummary, error) {
	return ListProjectWorkspaceReferences(r.db)
}

// GetWorkspaceSummary returns a single workspace's summary (project count +
// revision). See the package-level function's doc comment for the
// os.ErrNotExist contract.
func (r *ProjectRepository) GetWorkspaceSummary(slug string) (*WorkspaceSummary, error) {
	return GetWorkspaceSummary(r.db, slug)
}

// WorkspaceExists reports whether slug refers to an existing workspaces table row.
func (r *ProjectRepository) WorkspaceExists(slug string) (bool, error) {
	return WorkspaceExists(r.db, slug)
}

func (r *ProjectRepository) DeleteProject(id string) error {
	return DeleteProject(r.db, id)
}

// SetProjectUpstreamURL updates a project's captured upstream_url. See the
// package-level function for the underlying statement.
func (r *ProjectRepository) SetProjectUpstreamURL(id, upstreamURL string) error {
	return SetProjectUpstreamURL(r.db, id, upstreamURL)
}

// AssignDefaultWorkspaceToUnlinked inserts a project_workspaces row pointing
// at workspaceID for every project that does not yet have one. Returns the
// number of rows inserted. See the package-level function for the underlying
// statement.
func (r *ProjectRepository) AssignDefaultWorkspaceToUnlinked(workspaceID string) (int, error) {
	return AssignDefaultWorkspaceToUnlinked(r.db, workspaceID)
}

// TaskGCStore handles GC of tasks and their related data.
type TaskGCStore struct {
	conn           *sql.DB
	runtimesDir    string
	transcriptsDir string
	sandboxTmpDir  string
	// attachmentsRoot, when non-empty, is the data-home directory under which
	// per-task attachments live at `<root>/tasks/<id>/attachments`. GC
	// removes the per-task directory for tasks that have been in a terminal
	// state for olderThan. Empty disables this cleanup.
	attachmentsRoot string
	// RuntimeReaper, when set, is called with each runtime directory path
	// before os.RemoveAll removes it. Use this to Reap docker resources that
	// may still be alive in the upstream daemon (safety net for jobs whose
	// cleanupSandboxAfterWait did not complete, e.g. daemon restart).
	RuntimeReaper func(runtimeDir string) error
}

func NewTaskGCStore(conn *sql.DB) *TaskGCStore {
	return &TaskGCStore{conn: conn}
}

// WithRuntimesDir enables disk-level cleanup of per-sandbox runtime
// directories (`<dir>/<runtime_id>` and the git-gateway clone workspace dir
// `<dir>/<job_id>/workspace`) for GC target jobs. Empty disables runtime
// cleanup.
func (s *TaskGCStore) WithRuntimesDir(dir string) *TaskGCStore {
	s.runtimesDir = dir
	return s
}

// WithTranscriptsDir enables disk-level cleanup of the persistent
// transcript/diagnostics root (`<dir>/<runtime_id>`, holding transcript.log
// and diagnostics.json), keyed by the same runtime_id as WithRuntimesDir but
// a separate (persistent-volume) directory tree. Empty disables this
// cleanup, so those files accumulate forever.
func (s *TaskGCStore) WithTranscriptsDir(dir string) *TaskGCStore {
	s.transcriptsDir = dir
	return s
}

// WithRuntimeReaper sets a callback that is invoked with each runtime directory
// path before it is deleted. This allows the caller to Reap docker resources
// created by sandbox jobs (safety net when cleanupSandboxAfterWait didn't run,
// e.g. after a daemon restart).
func (s *TaskGCStore) WithRuntimeReaper(fn func(runtimeDir string) error) *TaskGCStore {
	s.RuntimeReaper = fn
	return s
}

// WithSandboxTmpDir enables safety-net cleanup of leaked /tmp/boid-* sandbox
// artifacts during GC. Pass the directory to scan (typically "/tmp"); empty
// string disables this cleanup.
func (s *TaskGCStore) WithSandboxTmpDir(dir string) *TaskGCStore {
	s.sandboxTmpDir = dir
	return s
}

// WithAttachmentsRoot enables disk-level cleanup of the per-task attachments
// directory tree rooted at `<dir>/tasks/<id>/attachments`. dir is the
// data-home (matches dataHomeFor in wire.go). Empty disables the cleanup.
func (s *TaskGCStore) WithAttachmentsRoot(dir string) *TaskGCStore {
	s.attachmentsRoot = dir
	return s
}

func (s *TaskGCStore) GC(olderThan time.Duration, dryRun bool) (*GCResult, error) {
	runtimesDeleted := 0
	if (s.runtimesDir != "" || s.transcriptsDir != "") && !dryRun {
		runtimesDeleted = s.cleanRuntimes(olderThan)
	}
	sandboxTmpDeleted := 0
	if s.sandboxTmpDir != "" && !dryRun {
		sandboxTmpDeleted = cleanSandboxTmp(s.sandboxTmpDir, olderThan)
	}
	if s.attachmentsRoot != "" && !dryRun {
		s.cleanTaskAttachments(olderThan)
	}

	var result *GCResult
	err := db.InTxDB(s.conn, func(dbtx db.DBTX) error {
		r, err := GCTasks(dbtx, terminalTaskStatusStrings(), olderThan, dryRun)
		if err != nil {
			return err
		}
		result = r
		// trigger_runs has no other retention — purge it in the same
		// transaction and schedule rather than a separate GC pass.
		n, err := GCTriggerRuns(dbtx, olderThan, dryRun)
		if err != nil {
			return err
		}
		result.TriggerRuns = n
		// Signal inbox GC rides the same transaction and schedule.
		sn, err := GCSignals(dbtx, olderThan, dryRun)
		if err != nil {
			return err
		}
		result.Signals = sn
		// card_requests has no other retention — purge it here too.
		cn, err := GCCardRequests(dbtx, olderThan, dryRun)
		if err != nil {
			return err
		}
		result.CardRequests = cn
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !dryRun {
		result.Runtimes = int64(runtimesDeleted)
		result.SandboxTmp = int64(sandboxTmpDeleted)
	}
	return result, nil
}

// cleanRuntimes deletes runtime directories for GC target jobs: the
// runtime_id-keyed sandbox scaffolding dir (`<RuntimesDir>/<runtime_id>`),
// its sibling in the persistent transcript root (`<TranscriptsDir>/
// <runtime_id>`), and the job.id-keyed git-gateway clone workspace dir
// (`<RuntimesDir>/<job.id>/workspace`) — the two use different directory
// naming schemes, so both must be checked. Covers both task-bound jobs
// (GC'd via the owning task's terminal status) and task-less ad-hoc jobs
// (GC'd by their own terminal status and updated_at). Errors are logged as
// warnings; failures do not block subsequent DB deletion. Returns the
// number of directories successfully deleted.
func (s *TaskGCStore) cleanRuntimes(olderThan time.Duration) int {
	// Uses the same terminal set as GCTasks (terminalStatusSQLList).
	query := `
		SELECT j.id, j.runtime_id
		FROM jobs j
		LEFT JOIN tasks t ON t.id = j.task_id
		WHERE (
		  (j.task_id IS NOT NULL AND t.status IN (` + terminalStatusSQLList + `))
		  OR
		  (j.task_id IS NULL AND j.status IN ('completed', 'failed'))
		)`
	var args []any
	if olderThan > 0 {
		query += ` AND COALESCE(t.updated_at, j.updated_at) < ?`
		args = append(args, time.Now().UTC().Add(-olderThan))
	}

	rows, err := s.conn.Query(query, args...)
	if err != nil {
		slog.Warn("gc runtimes: query failed", "error", err)
		return 0
	}
	defer rows.Close()

	type gcJob struct {
		id        string
		runtimeID string
	}
	var jobs []gcJob
	for rows.Next() {
		var j gcJob
		if err := rows.Scan(&j.id, &j.runtimeID); err != nil {
			slog.Warn("gc runtimes: scan failed", "error", err)
			return 0
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("gc runtimes: rows error", "error", err)
		return 0
	}

	count := 0
	seen := make(map[string]bool)
	remove := func(dir string, reap bool) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		// Reap docker resources before removing the directory so the ledger
		// is still readable. Only the runtime_id-keyed sandbox dir can hold
		// docker state.
		if reap && s.RuntimeReaper != nil {
			if err := s.RuntimeReaper(dir); err != nil {
				slog.Warn("gc docker reap failed", "dir", dir, "error", err)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("gc runtimes: remove failed", "dir", dir, "error", err)
			return
		}
		slog.Info("gc runtime removed", "dir", dir)
		count++
	}

	for _, j := range jobs {
		if j.runtimeID != "" {
			if s.runtimesDir != "" {
				remove(filepath.Join(s.runtimesDir, j.runtimeID), true)
			}
			// Persistent transcript-root sibling: never holds docker state.
			if s.transcriptsDir != "" {
				remove(filepath.Join(s.transcriptsDir, j.runtimeID), false)
			}
		}
		// job.id-keyed dir: houses the git-gateway clone workspace, only
		// created by clone-mode dispatch, so a no-op when absent.
		if s.runtimesDir != "" {
			remove(filepath.Join(s.runtimesDir, j.id), false)
		}
	}
	return count
}

// cleanTaskAttachments deletes the per-task data directory
// (`<attachmentsRoot>/tasks/<id>`) for tasks that have been in a terminal
// state for olderThan. The full per-task directory is removed, not just the
// attachments/ subdir, so future sibling data is also covered. Errors are
// logged as warnings; failures do not block subsequent DB deletion.
func (s *TaskGCStore) cleanTaskAttachments(olderThan time.Duration) {
	if s.attachmentsRoot == "" {
		return
	}
	query := `
		SELECT t.id
		FROM tasks t
		WHERE t.status IN (` + terminalStatusSQLList + `)`
	var args []any
	if olderThan > 0 {
		query += ` AND t.updated_at < ?`
		args = append(args, time.Now().UTC().Add(-olderThan))
	}

	rows, err := s.conn.Query(query, args...)
	if err != nil {
		slog.Warn("gc attachments: query failed", "error", err)
		return
	}
	defer rows.Close()

	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Warn("gc attachments: scan failed", "error", err)
			return
		}
		taskIDs = append(taskIDs, id)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("gc attachments: rows error", "error", err)
		return
	}

	for _, id := range taskIDs {
		dir := filepath.Join(s.attachmentsRoot, "tasks", id)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("gc attachments: remove failed", "task_id", id, "error", err)
			continue
		}
		slog.Info("gc attachments removed", "task_id", id)
	}
}

package orchestrator

// This file ingests boid's own action stream into the signal inbox (signals
// table). CreateAction (store.go) calls into it within the same transaction
// as the action INSERT. See docs/plans/boid-internal-signal-inbox.md for the
// full design.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// InternalSignalPack / InternalSignalConnector are the reserved "<pack>/
// <connector>" identity boid's own action-derived signals ingest under.
// InternalSignalPack is a reserved Pack name — internal/integrationpack's
// LoadPacks refuses to ever load a real installed Pack claiming it, so an
// external connector's rows can never collide with these under the same
// signals PRIMARY KEY (workspace_id, service, connector, id).
const (
	InternalSignalPack      = "boid"
	InternalSignalConnector = "actions"
)

// MetaProjectResolver answers, for a workspace, which of its projects
// declare signals.sources[] in their (hydrated) project.yaml — its
// metaprojects. Backed in production by *ProjectStore.MetaProjectIDs,
// injected here as a narrow interface rather than depending on
// *ProjectStore's full surface. See TaskRepository.SetMetaProjectResolver
// for the wiring seam.
type MetaProjectResolver interface {
	// MetaProjectIDs returns the project ids within workspaceID that declare
	// signals.sources[] — nil/empty when none do, including when the
	// resolver has no data at all for workspaceID; both mean "nothing to
	// ingest".
	MetaProjectIDs(workspaceID string) []string
}

// Compile-time proof that *ProjectStore satisfies this interface.
var _ MetaProjectResolver = (*ProjectStore)(nil)

type writerProjectContextKey struct{}

// WithWriterProjectID marks ctx as carrying a sandbox write's origin
// project — the same context-propagation idiom actor.go's
// WithActor/ActorFromContext established for Action.Actor, applied to the
// one additional fact CreateAction's ingest decision needs that Actor
// cannot supply: actor is stamped ActorHuman even for sandbox-originated
// calls, so it is not trustworthy evidence of the writer's project.
//
// internal/server/boid_executor.go's ExecuteBoidBuiltin is the only call
// site that should ever call this — every sandbox-originated write funnels
// through it with a sandbox.TokenContext carrying ProjectID, and every
// other ctx-building path in this codebase never touches this key.
//
// projectID may be "" — a sandbox write whose TokenContext.ProjectID could
// not be resolved still calls this (with an empty string), which is exactly
// what lets WriterProjectIDFromContext distinguish "no sandbox writer at
// all" from "sandbox writer, but its project is unknown" (fail-close).
func WithWriterProjectID(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, writerProjectContextKey{}, projectID)
}

// WriterProjectIDFromContext returns the project id WithWriterProjectID
// stamped, and whether ctx carries one at all. ok==false means this write
// was never routed through ExecuteBoidBuiltin (a human/daemon/HTTP-originated
// write) — CreateAction's actor-axis check treats that as "not a sandbox
// write, self-reference cannot apply". ok==true with an empty projectID
// means a sandbox write whose project could not be resolved — the fail-close case.
func WriterProjectIDFromContext(ctx context.Context) (projectID string, ok bool) {
	v := ctx.Value(writerProjectContextKey{})
	if v == nil {
		return "", false
	}
	projectID, ok = v.(string)
	return projectID, ok
}

// IngestActionSignal is CreateAction's internal-signal ingest step. Exported
// — rather than a private helper only CreateAction calls — so tests can
// exercise the ingest decision in isolation from the actions table's own id
// PRIMARY KEY (a real action id can only ever be INSERTed once, so a test
// proving re-ingest of the same action id is a no-op has to call this
// directly with a repeated Action value rather than through CreateAction
// twice).
//
// Best-effort by design. CreateAction calls this after its own INSERT has
// already run (as one statement within the caller's transaction) and only
// logs whatever error this returns: a failed ingest means the workspace
// misses one signal for this action, which is recoverable — the same
// card's next action carries its own signal. It must never fail the
// caller's transaction: the action is the fact, the signal is a side
// effect of it, and undoing the fact because the side effect failed would
// invert that causality.
func IngestActionSignal(ctx context.Context, dbtx db.DBTX, a *Action, resolver MetaProjectResolver) error {
	if resolver == nil || a == nil || a.TaskID == "" {
		return nil
	}

	taskType, projectID, err := actionTargetTypeAndProject(dbtx, a.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("internal signal ingest: resolve target task: %w", err)
	}
	// Ingest scope is card-type tasks only; exec-task actions (e.g.
	// api_gateway_request) are structurally excluded here.
	if taskType != TaskTypeCard {
		return nil
	}

	workspaceID, err := projectWorkspaceID(dbtx, projectID)
	if err != nil {
		return fmt.Errorf("internal signal ingest: resolve target project workspace: %w", err)
	}
	if workspaceID == "" {
		// Project is not linked to any workspace — no ingest target.
		return nil
	}

	metaProjectIDs := resolver.MetaProjectIDs(workspaceID)
	if len(metaProjectIDs) == 0 {
		// No project in the workspace declares signals.sources[] — nothing to ingest.
		return nil
	}

	// Self-reference guard + fail-close on the writer-project axis. The
	// actor string (task:<id> etc.) is never used as evidence here.
	if writerProjectID, hasWriter := WriterProjectIDFromContext(ctx); hasWriter {
		if writerProjectID == "" {
			// fail-close: a TokenContext existed but its project could not
			// be resolved.
			return nil
		}
		for _, mp := range metaProjectIDs {
			if mp == writerProjectID {
				// Self-reference: the metaproject's own job/task wrote this
				// action.
				return nil
			}
		}
	}

	row := SignalIngestRow{
		ID:         a.ID,
		OccurredAt: a.CreatedAt.UTC().Format(time.RFC3339Nano),
		Identity:   a.TaskID,
		Author:     a.Actor,
		Title:      a.Type,
	}
	return IngestSignals(dbtx, workspaceID, "", InternalSignalPack+"/"+InternalSignalConnector, []SignalIngestRow{row})
}

// actionTargetTypeAndProject is a minimal, single-row lookup (type + owning
// project id only) rather than the full GetTask (which additionally runs
// child-count rollup subqueries) — CreateAction's ingest step runs on every
// single action write, so it deliberately avoids that extra cost.
func actionTargetTypeAndProject(dbtx db.DBTX, taskID string) (taskType TaskType, projectID string, err error) {
	var t string
	err = dbtx.QueryRow(`SELECT type, project_id FROM tasks WHERE id = ?`, taskID).Scan(&t, &projectID)
	return TaskType(t), projectID, err
}

// projectWorkspaceID resolves projectID's workspace via project_workspaces
// (the same table action_list.go's WorkspaceID scoping joins against) —
// "" (with a nil error) when the project is not linked to any workspace,
// matching how the rest of this ingest step treats "nothing to ingest into"
// as a quiet no-op rather than an error.
func projectWorkspaceID(dbtx db.DBTX, projectID string) (string, error) {
	var workspaceID string
	err := dbtx.QueryRow(`SELECT workspace_id FROM project_workspaces WHERE project_id = ?`, projectID).Scan(&workspaceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return workspaceID, nil
}

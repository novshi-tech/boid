package orchestrator_test

// docs/plans/boid-internal-signal-inbox.md PR-1: CreateAction's internal-
// signal ingest step. Pins the §10 グループB 採点表 (Q5, Q7-Q12) directly —
// Q6 (SweepReconcileChildren → recordChildClosedOnParent が実際に
// CreateAction を通ること) lives in internal/api/queue_sweep_test.go instead,
// since that call path is owned by that package.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// stubMetaResolver is a MetaProjectResolver test double: workspaceID ->
// metaproject ids, set up per test.
type stubMetaResolver map[string][]string

func (r stubMetaResolver) MetaProjectIDs(workspaceID string) []string {
	return r[workspaceID]
}

// seedProject inserts a projects row (and, unless workspaceID is "", a
// project_workspaces row linking it) — the minimum CreateAction's ingest
// step reads via actionTargetTypeAndProject/projectWorkspaceID.
func seedProject(t *testing.T, dbtx db.DBTX, projectID, workspaceID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		projectID, "/tmp/"+projectID, now, now,
	); err != nil {
		t.Fatalf("insert project %q: %v", projectID, err)
	}
	if workspaceID == "" {
		return
	}
	if _, err := dbtx.Exec(
		`INSERT INTO project_workspaces (project_id, workspace_id) VALUES (?, ?)`,
		projectID, workspaceID,
	); err != nil {
		t.Fatalf("insert project_workspaces %q/%q: %v", projectID, workspaceID, err)
	}
}

// seedCardTask inserts a type='card' task owned by projectID.
func seedCardTask(t *testing.T, dbtx db.DBTX, taskID, projectID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := dbtx.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, kind, urgency, wake_task_id, suggestion_verb, detail, created_at, updated_at)
		 VALUES (?, 'card', ?, ?, 'working', '', '', '', '', '{}', ?, ?)`,
		taskID, projectID, "card "+taskID, now, now,
	); err != nil {
		t.Fatalf("insert card task %q: %v", taskID, err)
	}
}

// seedExecutionTask inserts a type='execution' task owned by projectID.
func seedExecutionTask(t *testing.T, dbtx db.DBTX, taskID, projectID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := dbtx.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start, created_at, updated_at)
		 VALUES (?, 'execution', ?, ?, 'executing', '', '[]', FALSE, '', '', '{}', '[]', FALSE, ?, ?)`,
		taskID, projectID, "exec "+taskID, now, now,
	); err != nil {
		t.Fatalf("insert execution task %q: %v", taskID, err)
	}
}

func listInternalSignals(t *testing.T, dbtx db.DBTX, workspaceID string) []*orchestrator.Signal {
	t.Helper()
	signals, err := orchestrator.ListSignals(dbtx, orchestrator.SignalFilter{WorkspaceID: workspaceID, State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	return signals
}

func newAction(id, taskID, actionType, actor string) *orchestrator.Action {
	return &orchestrator.Action{
		ID:        id,
		TaskID:    taskID,
		Type:      actionType,
		Actor:     actor,
		CreatedAt: time.Now().UTC(),
	}
}

// --- Q5: signals未宣言workspaceでは既存の挙動が1ビットも変わらない ---

func TestIngestActionSignal_NoMetaProjectInWorkspace_NoSignalRow(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-a")

	resolver := stubMetaResolver{} // ws-1 に一切登録なし
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorHuman)
	if err := orchestrator.IngestActionSignal(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 0 {
		t.Fatalf("got %d signals, want 0 (no metaproject declared in ws-1)", len(got))
	}
}

// TestCreateAction_NoMetaProjectResolver_ActionStillWritesNormally proves the
// resolver being entirely unwired (nil) — every pre-PR-1 caller/test — is
// indistinguishable from "no metaproject": the action row itself is
// unaffected either way.
func TestCreateAction_NilResolver_ActionStillWritesNormally(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-a")

	a := &orchestrator.Action{TaskID: "card-1", Type: "attrs_set", Actor: orchestrator.ActorHuman}
	if err := orchestrator.CreateAction(context.Background(), d.Conn, a, nil); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if a.ID == "" {
		t.Error("CreateAction did not assign an id")
	}
	actions, err := orchestrator.ListActionsByTask(d.Conn, "card-1")
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
}

// --- Q7: fail-close (TokenContext ありだが project を解決できないなら
// ingest しない) ---

func TestIngestActionSignal_FailCloseWhenWriterProjectUnresolved(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-a")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	// TokenContext は持つが ProjectID が空 = 解決できなかったケース。
	ctx := orchestrator.WithWriterProjectID(context.Background(), "")
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask(""))
	if err := orchestrator.IngestActionSignal(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 0 {
		t.Fatalf("got %d signals, want 0 (fail-close: writer project unresolved)", len(got))
	}
}

// --- Q8: 自己参照は actor 文字列でなく project で判定される。task: (id空)
// からの書き込みも正しく落ちる ---

func TestIngestActionSignal_SelfReferenceBlockedByProjectNotActorString(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	// exec job (id空の task:) からの書き込み — actor 文字列は "task:" だが
	// 遮断の根拠は WithWriterProjectID の project 側でなければならない。
	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-meta")
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask(""))
	if err := orchestrator.IngestActionSignal(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 0 {
		t.Fatalf("got %d signals, want 0 (self-reference: writer project IS the metaproject)", len(got))
	}
}

// TestIngestActionSignal_ActorStringAloneNeverBlocks proves the converse of
// Q8: an actor string that LOOKS like a self-authored write (task:<id> with
// no id, matching khi's old decide-by-actor trap) must NOT be blocked when
// the writer's project (from ctx) is NOT a metaproject — actor is never
// consulted for this decision.
func TestIngestActionSignal_ActorStringAloneNeverBlocks(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedProject(t, d.Conn, "proj-other", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	// Actor looks exactly like khi's own decide-by-actor trigger job marker
	// (ActorTask("")), but this write's ctx says it came from a DIFFERENT
	// project's exec job — must ingest.
	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-other")
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask(""))
	if err := orchestrator.IngestActionSignal(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 1 {
		t.Fatalf("got %d signals, want 1 (actor string alone must not block ingest)", len(got))
	}
}

// --- Q9: card 宛でない action (例 api_gateway_request) は inbox に入らない ---

func TestIngestActionSignal_NonCardTarget_NotIngested(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedProject(t, d.Conn, "proj-a", "ws-1")
	seedExecutionTask(t, d.Conn, "exec-1", "proj-a")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	a := newAction("act-1", "exec-1", "api_gateway_request", orchestrator.ActorTask("exec-1"))
	if err := orchestrator.IngestActionSignal(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 0 {
		t.Fatalf("got %d signals, want 0 (execution task is never a card, never ingested)", len(got))
	}
}

// --- Q10: 同じ action の2度 ingest は no-op (dedup) ---

func TestIngestActionSignal_SameActionTwice_IsNoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")
	// proj-other authors the write (not the metaproject) so it ingests.
	seedProject(t, d.Conn, "proj-other", "ws-1")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-other")
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask("t-1"))

	if err := orchestrator.IngestActionSignal(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if err := orchestrator.IngestActionSignal(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	got := listInternalSignals(t, d.Conn, "ws-1")
	if len(got) != 1 {
		t.Fatalf("got %d signals after 2 ingests of the same action, want 1 (PK dedup)", len(got))
	}
	if got[0].ID != "act-1" {
		t.Errorf("Signal.ID = %q, want %q", got[0].ID, "act-1")
	}
}

// --- Q11: signal INSERT 失敗時も action 書き込みは成立する ---

func TestCreateAction_IngestFailure_ActionStillCommits(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")
	seedProject(t, d.Conn, "proj-other", "ws-1")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-other")
	// An oversized Actor (mapped to the envelope's Author field) deterministically
	// fails IngestSignals' own ValidateContentSize check (signal_store.go,
	// 64KiB) BEFORE it ever issues the row's INSERT — a real internal error,
	// and one that fails closed (no partial signal row) rather than partway
	// through, so this test's "0 signals" assertion below actually proves
	// the ingest failed rather than merely succeeding oddly. Actor is not
	// size-validated on the action's own INSERT path, so the action write
	// itself is unaffected.
	a := &orchestrator.Action{TaskID: "card-1", Type: "attrs_set", Actor: strings.Repeat("x", orchestrator.MaxContentBytes+1)}

	if err := orchestrator.CreateAction(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("CreateAction returned an error (ingest failure must not fail the action write): %v", err)
	}
	actions, err := orchestrator.ListActionsByTask(d.Conn, "card-1")
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1 (action must commit even though ingest failed)", len(actions))
	}
	// And the ingest genuinely did fail (not a false-negative test) — no
	// signal landed.
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 0 {
		t.Fatalf("got %d signals, want 0 (this test's premise is that ingest fails)", len(got))
	}
}

// --- Q12: 複数のメタプロジェクトがあっても全部が自己参照として落ちる ---

func TestIngestActionSignal_MultipleMetaProjects_AllBlockedAsSelfReference(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta-a", "ws-1")
	seedProject(t, d.Conn, "proj-meta-b", "ws-1")
	seedProject(t, d.Conn, "proj-other", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta-a")
	seedCardTask(t, d.Conn, "card-2", "proj-meta-b")

	resolver := stubMetaResolver{"ws-1": {"proj-meta-a", "proj-meta-b"}}

	// proj-meta-a writing to card-1 (its own card) — self-reference.
	ctxA := orchestrator.WithWriterProjectID(context.Background(), "proj-meta-a")
	if err := orchestrator.IngestActionSignal(ctxA, d.Conn, newAction("act-a", "card-1", "attrs_set", orchestrator.ActorTask("")), resolver); err != nil {
		t.Fatalf("ingest a: %v", err)
	}
	// proj-meta-b writing to card-2 (its own card) — self-reference too,
	// second entry in the metaproject set.
	ctxB := orchestrator.WithWriterProjectID(context.Background(), "proj-meta-b")
	if err := orchestrator.IngestActionSignal(ctxB, d.Conn, newAction("act-b", "card-2", "attrs_set", orchestrator.ActorTask("")), resolver); err != nil {
		t.Fatalf("ingest b: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 0 {
		t.Fatalf("got %d signals, want 0 (both metaprojects must be excluded as self-reference)", len(got))
	}

	// A third, non-meta project writing to card-1 DOES ingest — proves the
	// exclusion is scoped to the metaproject set, not "block everything".
	ctxOther := orchestrator.WithWriterProjectID(context.Background(), "proj-other")
	if err := orchestrator.IngestActionSignal(ctxOther, d.Conn, newAction("act-c", "card-1", "attrs_set", orchestrator.ActorTask("")), resolver); err != nil {
		t.Fatalf("ingest c: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 1 {
		t.Fatalf("got %d signals, want 1 (a non-metaproject writer must still ingest)", len(got))
	}
}

// --- 人 / daemon (TokenContext なし) は通常どおり ingest される ---

func TestIngestActionSignal_HumanOrDaemonWrite_Ingested(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	// No WithWriterProjectID at all — matches every HTTP/daemon-loop ctx in
	// production (WithActor(r.Context(), ActorHuman) etc. never touch this
	// key).
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorHuman)
	if err := orchestrator.IngestActionSignal(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 1 {
		t.Fatalf("got %d signals, want 1 (human/daemon writes are never self-referencing)", len(got))
	}
}

// --- envelope 写像 (§4.6) ---

func TestIngestActionSignal_EnvelopeMapping(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")

	resolver := stubMetaResolver{"ws-1": {"proj-meta"}}
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorHuman)
	if err := orchestrator.IngestActionSignal(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestActionSignal: %v", err)
	}
	got := listInternalSignals(t, d.Conn, "ws-1")
	if len(got) != 1 {
		t.Fatalf("got %d signals, want 1", len(got))
	}
	sig := got[0]
	if sig.ID != "act-1" {
		t.Errorf("ID = %q, want action id %q", sig.ID, "act-1")
	}
	if sig.Connector != orchestrator.InternalSignalPack+"/"+orchestrator.InternalSignalConnector {
		t.Errorf("Connector = %q, want %q", sig.Connector, orchestrator.InternalSignalPack+"/"+orchestrator.InternalSignalConnector)
	}
	if sig.Service != "" {
		t.Errorf("Service = %q, want empty (no external service reached)", sig.Service)
	}
	if sig.Identity != "card-1" {
		t.Errorf("Identity = %q, want target task id %q", sig.Identity, "card-1")
	}
	if sig.Author != orchestrator.ActorHuman {
		t.Errorf("Author = %q, want %q", sig.Author, orchestrator.ActorHuman)
	}
	if sig.Title != "attrs_set" {
		t.Errorf("Title = %q, want action type %q", sig.Title, "attrs_set")
	}
}

// --- MetaProjectResolver: *ProjectStore.MetaProjectIDs ---

func TestProjectStore_MetaProjectIDs(t *testing.T) {
	s := orchestrator.NewProjectStore()
	s.Set("proj-meta", &orchestrator.ProjectMeta{
		ID: "proj-meta",
		Signals: orchestrator.SignalsConfig{
			Sources: []orchestrator.SignalSource{{Connector: "slack/mentions", Service: "slack-api", Every: "10m"}},
		},
	})
	s.SetWorkspaceID("proj-meta", "ws-1")
	s.Set("proj-plain", &orchestrator.ProjectMeta{ID: "proj-plain"})
	s.SetWorkspaceID("proj-plain", "ws-1")
	s.Set("proj-other-ws", &orchestrator.ProjectMeta{
		ID:      "proj-other-ws",
		Signals: orchestrator.SignalsConfig{Sources: []orchestrator.SignalSource{{Connector: "slack/mentions", Service: "slack-api", Every: "10m"}}},
	})
	s.SetWorkspaceID("proj-other-ws", "ws-2")

	got := s.MetaProjectIDs("ws-1")
	if len(got) != 1 || got[0] != "proj-meta" {
		t.Fatalf("MetaProjectIDs(ws-1) = %v, want [proj-meta]", got)
	}
	if got := s.MetaProjectIDs("ws-2"); len(got) != 1 || got[0] != "proj-other-ws" {
		t.Fatalf("MetaProjectIDs(ws-2) = %v, want [proj-other-ws]", got)
	}
	if got := s.MetaProjectIDs("ws-none"); len(got) != 0 {
		t.Fatalf("MetaProjectIDs(ws-none) = %v, want empty", got)
	}
}

// --- WithWriterProjectID / WriterProjectIDFromContext round-trip ---

func TestWriterProjectIDFromContext_RoundTrip(t *testing.T) {
	if _, ok := orchestrator.WriterProjectIDFromContext(context.Background()); ok {
		t.Fatal("plain context.Background() must report ok=false (no sandbox writer)")
	}
	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-x")
	id, ok := orchestrator.WriterProjectIDFromContext(ctx)
	if !ok || id != "proj-x" {
		t.Fatalf("WriterProjectIDFromContext = (%q, %v), want (%q, true)", id, ok, "proj-x")
	}
	// Empty project id (unresolved) is still a distinct "has a sandbox
	// writer" state from no key at all.
	ctxEmpty := orchestrator.WithWriterProjectID(context.Background(), "")
	id, ok = orchestrator.WriterProjectIDFromContext(ctxEmpty)
	if !ok || id != "" {
		t.Fatalf("WriterProjectIDFromContext(empty) = (%q, %v), want (\"\", true)", id, ok)
	}
	// Composing with WithActor (the sibling context key) must not clobber
	// either value.
	ctxBoth := orchestrator.WithActor(ctx, orchestrator.ActorTask("t-1"))
	id, ok = orchestrator.WriterProjectIDFromContext(ctxBoth)
	if !ok || id != "proj-x" {
		t.Fatalf("WriterProjectIDFromContext after WithActor = (%q, %v), want (%q, true)", id, ok, "proj-x")
	}
	if orchestrator.ActorFromContext(ctxBoth) != orchestrator.ActorTask("t-1") {
		t.Fatalf("ActorFromContext after composing = %q, want %q", orchestrator.ActorFromContext(ctxBoth), orchestrator.ActorTask("t-1"))
	}
}

// --- TaskRepository.CreateAction: real wiring, InTxDB dual-mode shape ---

func TestTaskRepository_CreateAction_IngestsWhenResolverWired(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")
	seedProject(t, d.Conn, "proj-other", "ws-1")

	repo := orchestrator.NewTaskRepository(d.Conn)
	repo.SetMetaProjectResolver(stubMetaResolver{"ws-1": {"proj-meta"}})

	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-other")
	if err := repo.CreateAction(ctx, &orchestrator.Action{TaskID: "card-1", Type: "attrs_set", Actor: orchestrator.ActorTask("t-1")}); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 1 {
		t.Fatalf("got %d signals, want 1", len(got))
	}
}

func TestTaskRepository_CreateAction_NestsInsideExistingTx(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-meta", "ws-1")
	seedCardTask(t, d.Conn, "card-1", "proj-meta")
	seedProject(t, d.Conn, "proj-other", "ws-1")

	ctx := orchestrator.WithWriterProjectID(context.Background(), "proj-other")
	err := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		repo := orchestrator.NewTaskRepository(tx)
		repo.SetMetaProjectResolver(stubMetaResolver{"ws-1": {"proj-meta"}})
		return repo.CreateAction(ctx, &orchestrator.Action{TaskID: "card-1", Type: "attrs_set", Actor: orchestrator.ActorTask("t-1")})
	})
	if err != nil {
		t.Fatalf("InTxDB: %v", err)
	}
	if got := listInternalSignals(t, d.Conn, "ws-1"); len(got) != 1 {
		t.Fatalf("got %d signals, want 1", len(got))
	}
}

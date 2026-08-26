package api

// docs/plans/signal-ingest-detailed-design.md §3.1 (PR-2): tests for the
// host API surface (GET /api/signals, POST /api/signals/ack). Uses a real
// orchestrator.TaskRepository backed by an in-memory sqlite DB
// (db.Open(":memory:") + migrate.Apply — NOT testutil.NewTestDB: importing
// testutil here would cycle through internal/server back into internal/api,
// same reason action_list_read_test.go gives) rather than a fake
// SignalStore — PR-1's store layer already has its own exhaustive tests
// (internal/orchestrator/signal_store_test.go); this file's job is to prove
// the HANDLER wiring (query param parsing, workspace default, JSON shaping,
// error mapping) against the real thing, not to re-derive store semantics.
//
// signal-driven-review.md §14 Q14 ("`boid signal ack` は冪等で、二重 ack が
// エラーにならないテストがある") is pinned at BOTH this layer
// (TestSignalHandler_Ack_Idempotent) and the CLI layer (cmd/signal_test.go).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newSignalHandlerTest(t *testing.T) (*SignalHandler, *orchestrator.TaskRepository) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := orchestrator.NewTaskRepository(d.Conn)
	return &SignalHandler{Store: repo}, repo
}

func seedSignal(t *testing.T, repo *orchestrator.TaskRepository, ws, service, connector string, row orchestrator.SignalIngestRow) {
	t.Helper()
	if err := repo.IngestSignals(ws, service, connector, []orchestrator.SignalIngestRow{row}); err != nil {
		t.Fatalf("seed signal %q: %v", row.ID, err)
	}
}

// TestSignalHandler_List_DefaultsWorkspaceWhenOmitted pins that a caller
// hitting GET /api/signals with no workspace_id at all still gets a scoped
// result (the "default" slug) instead of ErrActionListUnscoped-style
// rejection — the handler-side half of the CLI's own --workspace default
// (signal-ingest-detailed-design.md §3.1).
func TestSignalHandler_List_DefaultsWorkspaceWhenOmitted(t *testing.T) {
	h, repo := newSignalHandlerTest(t)
	seedSignal(t, repo, orchestrator.DefaultWorkspaceSlug, "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T02:23:48Z", Identity: "slack-thread:1",
	})
	// A signal in a NON-default workspace must not leak into the
	// default-scoped list.
	seedSignal(t, repo, "other-ws", "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:2", OccurredAt: "2026-08-26T02:24:00Z", Identity: "slack-thread:2",
	})

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	var got ListSignalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, rec.Body)
	}
	if len(got.Signals) != 1 || got.Signals[0].ID != "slack:1" {
		t.Fatalf("signals = %+v, want exactly the default-workspace signal", got.Signals)
	}
}

// TestSignalHandler_List_Filters exercises source/service/state/limit
// together, and pins that the envelope's `source` block is the
// pack/connector SPLIT of the stored "<pack>/<connector>" composite
// (signal-driven-review.md §5.2), not the raw composite string.
func TestSignalHandler_List_Filters(t *testing.T) {
	h, repo := newSignalHandlerTest(t)
	ws := "ws-1"
	seedSignal(t, repo, ws, "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1", Title: "hello",
	})
	seedSignal(t, repo, ws, "jira-api", "jira-cloud/jira-cloud", orchestrator.SignalIngestRow{
		ID: "jira:1", OccurredAt: "2026-08-26T02:00:00Z", Identity: "jira:PROJ-1",
	})
	seedSignal(t, repo, ws, "jira-api", "jira-cloud/jira-cloud", orchestrator.SignalIngestRow{
		ID: "jira:2", OccurredAt: "2026-08-26T03:00:00Z", Identity: "jira:PROJ-2",
	})

	t.Run("source filters to one connector and splits pack/connector", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?workspace_id="+ws+"&source=slack/mentions&state=all", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
		}
		var got ListSignalsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Signals) != 1 {
			t.Fatalf("signals = %+v, want exactly 1 (slack/mentions only)", got.Signals)
		}
		s := got.Signals[0]
		if s.Source.Pack != "slack" || s.Source.Connector != "mentions" || s.Source.Service != "slack-api" {
			t.Fatalf("source = %+v, want {pack:slack connector:mentions service:slack-api}", s.Source)
		}
		if s.Title != "hello" {
			t.Fatalf("title = %q, want %q", s.Title, "hello")
		}
	})

	t.Run("service filters within a workspace", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?workspace_id="+ws+"&service=jira-api&state=all", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
		}
		var got ListSignalsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Signals) != 2 {
			t.Fatalf("signals = %+v, want exactly 2 (jira-api only)", got.Signals)
		}
	})

	t.Run("limit caps the result", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?workspace_id="+ws+"&state=all&limit=1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
		}
		var got ListSignalsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Signals) != 1 {
			t.Fatalf("signals = %+v, want exactly 1 (limit=1)", got.Signals)
		}
		// occurred_at ASC ordering (signal_store.go) means the earliest row
		// (slack:1) comes back under limit=1.
		if got.Signals[0].ID != "slack:1" {
			t.Fatalf("signals[0].ID = %q, want slack:1 (oldest-first)", got.Signals[0].ID)
		}
	})

	t.Run("state=pending is the default and excludes acked", func(t *testing.T) {
		if err := repo.AckSignals(ws, []string{"jira:1"}); err != nil {
			t.Fatalf("ack: %v", err)
		}
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?workspace_id="+ws, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
		}
		var got ListSignalsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, s := range got.Signals {
			if s.ID == "jira:1" {
				t.Fatalf("signals = %+v, want jira:1 excluded (acked, default state=pending)", got.Signals)
			}
		}
		if len(got.Signals) != 2 {
			t.Fatalf("signals = %+v, want exactly 2 pending (slack:1, jira:2)", got.Signals)
		}
	})
}

// TestSignalHandler_Ack_Idempotent pins Q14 at the handler layer: acking the
// same id twice must succeed both times, and the SECOND call (a genuine
// no-op against the store) must still return 200 with the id listed as
// acked.
func TestSignalHandler_Ack_Idempotent(t *testing.T) {
	h, repo := newSignalHandlerTest(t)
	ws := "ws-1"
	seedSignal(t, repo, ws, "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	ackBody := func() *strings.Reader {
		return strings.NewReader(`{"workspace_id":"` + ws + `","ids":["slack:1"]}`)
	}

	for i, label := range []string{"first ack", "second ack (idempotent)"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/ack", ackBody())
		h.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s (call %d): code = %d, body = %s", label, i, rec.Code, rec.Body)
		}
		var got AckSignalsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
		if len(got.Acked) != 1 || got.Acked[0] != "slack:1" {
			t.Fatalf("%s: acked = %+v, want [slack:1]", label, got.Acked)
		}
	}
}

// TestSignalHandler_Ack_DefaultsWorkspaceWhenOmitted mirrors the List
// default, on the write side.
func TestSignalHandler_Ack_DefaultsWorkspaceWhenOmitted(t *testing.T) {
	h, repo := newSignalHandlerTest(t)
	seedSignal(t, repo, orchestrator.DefaultWorkspaceSlug, "slack-api", "slack/mentions", orchestrator.SignalIngestRow{
		ID: "slack:1", OccurredAt: "2026-08-26T01:00:00Z", Identity: "slack-thread:1",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ack", strings.NewReader(`{"ids":["slack:1"]}`))
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}

	signals, err := repo.ListSignals(orchestrator.SignalFilter{WorkspaceID: orchestrator.DefaultWorkspaceSlug, State: orchestrator.SignalStateAcked})
	if err != nil {
		t.Fatalf("list acked: %v", err)
	}
	if len(signals) != 1 || signals[0].ID != "slack:1" {
		t.Fatalf("acked signals in default workspace = %+v, want [slack:1]", signals)
	}
}

// TestSignalHandler_Ack_UnknownID_Errors pins the store layer's typo
// detection (signal-ingest-detailed-design.md §2: "未知 id はエラーで列挙")
// surfaces as a non-2xx response, not a silent 200.
func TestSignalHandler_Ack_UnknownID_Errors(t *testing.T) {
	h, _ := newSignalHandlerTest(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ack", strings.NewReader(`{"workspace_id":"ws-1","ids":["no-such-id"]}`))
	h.Routes().ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("code = %d, want an error status for an unknown id", rec.Code)
	}
}

// TestSignalHandler_Ack_EmptyIDs_Errors pins that an empty ids list is
// rejected up front rather than silently no-op-succeeding (AckSignals
// itself treats len(ids)==0 as a no-op success, but a caller sending an
// empty ack request is almost certainly a client bug worth surfacing).
func TestSignalHandler_Ack_EmptyIDs_Errors(t *testing.T) {
	h, _ := newSignalHandlerTest(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ack", strings.NewReader(`{"workspace_id":"ws-1","ids":[]}`))
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for an empty ids list", rec.Code)
	}
}

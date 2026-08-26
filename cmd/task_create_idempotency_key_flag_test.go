package cmd

// TestTaskCreate_IdempotencyKeyFlag_SentOnWire pins `boid task create
// --idempotency-key <key>` (docs/plans/signal-ingest-detailed-design.md §8):
// the flag must land on the POST /api/tasks request body as
// idempotency_key, exactly like it would if the caller had written
// `idempotency_key: <key>` into the YAML spec directly (already covered by
// TestParseTaskCreateSpec_AllTopLevelFields). This is the CLI leg of the "3
//経路すべて" wiring; the actual get-or-create semantics are pinned at the
// host-API/store layers (internal/api/task_create_idempotency_key_test.go,
// internal/orchestrator/idempotency_key_store_test.go) since the CLI is a
// thin translator to POST /api/tasks and never talks to the DB itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

func TestTaskCreate_IdempotencyKeyFlag_SentOnWire(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"task-1","status":"parked"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build a client for %s: %v", srv.URL, err)
	}

	cmd := taskCreateCmd
	prev := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		cmd.SetIn(nil)
		_ = cmd.Flags().Set("idempotency-key", "")
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))
	cmd.SetIn(strings.NewReader("project_id: proj-1\ntitle: Child task\ninitial_status: parked\n"))
	if err := cmd.Flags().Set("idempotency-key", "card-1:child-gen-1"); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	if err := runTaskCreate(cmd, nil); err != nil {
		t.Fatalf("runTaskCreate: %v", err)
	}

	if gotBody == nil {
		t.Fatal("daemon never received a request body")
	}
	if gotBody["idempotency_key"] != "card-1:child-gen-1" {
		t.Errorf("idempotency_key on wire = %v, want %q", gotBody["idempotency_key"], "card-1:child-gen-1")
	}
}

// TestTaskCreate_IdempotencyKeyFlag_OmittedLeavesSpecFieldAlone pins that the
// flag is purely additive: when --idempotency-key is not passed, a spec that
// already sets idempotency_key in its YAML body must survive untouched
// (regression against an empty-string flag value stomping the spec field).
func TestTaskCreate_IdempotencyKeyFlag_OmittedLeavesSpecFieldAlone(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"task-1","status":"parked"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build a client for %s: %v", srv.URL, err)
	}

	cmd := taskCreateCmd
	prev := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		cmd.SetIn(nil)
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))
	cmd.SetIn(strings.NewReader("project_id: proj-1\ntitle: Child task\ninitial_status: parked\nidempotency_key: from-spec\n"))

	if err := runTaskCreate(cmd, nil); err != nil {
		t.Fatalf("runTaskCreate: %v", err)
	}

	if gotBody["idempotency_key"] != "from-spec" {
		t.Errorf("idempotency_key on wire = %v, want %q (spec value preserved when flag omitted)", gotBody["idempotency_key"], "from-spec")
	}
}

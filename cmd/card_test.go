package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

// TestCardRun_Success pins the happy path: `boid card run` POSTs to
// /api/cards/{id}/commands/{key} with the --instruction body and renders
// the launcher's own request_id/launcher_job_id.
func TestCardRun_Success(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"occupied":false,"request_id":"req-1","launcher_job_id":"job-1"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := cardRunCmd
	prev := cmd.Context()
	prevInstr := cardRunInstruction
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		cardRunInstruction = prevInstr
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))
	cardRunInstruction = "look into this"

	if err := cmd.RunE(cmd, []string{"card-1", "review"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/cards/card-1/commands/review" {
		t.Errorf("path = %q, want /api/cards/card-1/commands/review", gotPath)
	}
	if gotBody["instruction"] != "look into this" {
		t.Errorf("body instruction = %q, want %q", gotBody["instruction"], "look into this")
	}
	if !bytes.Contains(out.Bytes(), []byte("req-1")) || !bytes.Contains(out.Bytes(), []byte("job-1")) {
		t.Errorf("output missing request_id/launcher_job_id: %s", out.String())
	}
}

// TestCardRun_Occupied_ShowsLinkAndPreservesInstruction pins that an
// occupied response renders the current occupant's target AND echoes the
// caller's own instruction back, so the CLI never silently drops it.
func TestCardRun_Occupied_ShowsLinkAndPreservesInstruction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"occupied":true,"request_id":"req-1","target_kind":"task","target_id":"task-1","instruction":"go do it"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := cardRunCmd
	prev := cmd.Context()
	prevInstr := cardRunInstruction
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		cardRunInstruction = prevInstr
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))
	cardRunInstruction = "go do it"

	if err := cmd.RunE(cmd, []string{"card-1", "review"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := out.String()
	if !bytes.Contains(out.Bytes(), []byte("task task-1")) {
		t.Errorf("output missing occupant link: %s", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("go do it")) {
		t.Errorf("output missing preserved instruction: %s", got)
	}
}

// TestCardRequests_ListsRows pins that `boid card requests <card-id>` GETs
// /api/card-requests?card_id=<id> and renders a Go request's command_key as
// "(go)" rather than the raw sentinel value.
func TestCardRequests_ListsRows(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"id":"req-1","card_id":"card-1","command_key":"review","status":"attached","target_kind":"task","target_id":"task-1","created_at":"2026-01-01T00:00:00Z"},
			{"id":"req-2","card_id":"card-1","command_key":"__go__","status":"finished","created_at":"2026-01-02T00:00:00Z"}
		]`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := cardRequestsCmd
	prev := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))

	if err := cmd.RunE(cmd, []string{"card-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if gotPath != "/api/card-requests?card_id=card-1" {
		t.Errorf("path/query = %q, want /api/card-requests?card_id=card-1", gotPath)
	}
	got := out.String()
	if !bytes.Contains(out.Bytes(), []byte("req-1")) || !bytes.Contains(out.Bytes(), []byte("task:task-1")) {
		t.Errorf("output missing req-1/target: %s", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("(go)")) {
		t.Errorf("output must render the Go sentinel command_key as (go): %s", got)
	}
}

// TestCardRequests_Empty_PrintsClearMessage confirms the empty case is not
// silent.
func TestCardRequests_Empty_PrintsClearMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := cardRequestsCmd
	prev := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))

	if err := cmd.RunE(cmd, []string{"card-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no card_requests")) {
		t.Errorf("expected a clear empty message, got: %s", out.String())
	}
}

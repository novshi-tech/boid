package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

// TestTaskDiagnoseCards_ListsOnlyViolatingCards pins the read-only
// diagnostic: a card with two open children is flagged, a healthy
// single-child card is not, and no state-changing request is ever made
// (the httptest server only ever serves GET).
func TestTaskDiagnoseCards_ListsOnlyViolatingCards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected non-GET request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("parent_id") != "" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[
			{
				"id": "card-bad", "type": "card", "title": "two unfinished children", "status": "working",
				"card": {"detail": {"children":[{"id":"c1","status":"open"},{"id":"c2","status":"specced"}]}}
			},
			{
				"id": "card-good", "type": "card", "title": "one child, healthy", "status": "working",
				"card": {"detail": {"children":[{"id":"c1","status":"specced"}]}}
			}
		]`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskDiagnoseCardsCmd
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

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := out.String()
	if !bytes.Contains(out.Bytes(), []byte("card-bad")) {
		t.Errorf("output missing the violating card: %s", got)
	}
	if bytes.Contains(out.Bytes(), []byte("card-good")) {
		t.Errorf("output must not list the healthy card: %s", got)
	}
}

// TestTaskDiagnoseCards_SpeccedChildsOwnLiveRow_NotFlagged pins that a
// specced child's own live task row (Ref matching the JSON child id) is not
// double-counted as a second occupant — matching acceptGo's own accounting.
func TestTaskDiagnoseCards_SpeccedChildsOwnLiveRow_NotFlagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("parent_id") != "" {
			fmt.Fprint(w, `[{"id":"t1","type":"execution","status":"pending","ref":"c1"}]`)
			return
		}
		fmt.Fprint(w, `[{"id":"card-1","type":"card","title":"healthy","status":"working","card":{"detail":{"children":[{"id":"c1","status":"specced"}]}}}]`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskDiagnoseCardsCmd
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

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if bytes.Contains(out.Bytes(), []byte("card-1")) {
		t.Errorf("card-1's own reserved child must not be flagged as a violation: %s", out.String())
	}
}

// TestTaskDiagnoseCards_ActiveCardRequest_FlaggedEvenWithNoUnresolvedChildren
// pins that a card with zero unresolved children but an active
// card_requests row is still surfaced — a child-count-only reader would
// otherwise report it as "free" when a command or Go dispatch already
// occupies its slot.
func TestTaskDiagnoseCards_ActiveCardRequest_FlaggedEvenWithNoUnresolvedChildren(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/card-requests"):
			fmt.Fprint(w, `[{"id":"req-1","card_id":"card-1","status":"launching"}]`)
		case r.URL.Query().Get("parent_id") != "":
			fmt.Fprint(w, `[]`)
		default:
			fmt.Fprint(w, `[{"id":"card-1","type":"card","title":"mid-command","status":"working","card":{"detail":{}}}]`)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskDiagnoseCardsCmd
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

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("card-1")) || !bytes.Contains(out.Bytes(), []byte("req-1")) {
		t.Errorf("expected card-1 flagged with its active card_request req-1, got: %s", out.String())
	}
}

// TestTaskDiagnoseCards_NoViolations_PrintsClearMessage confirms the empty
// case is not silent.
func TestTaskDiagnoseCards_NoViolations_PrintsClearMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"card-good","type":"card","title":"fine","status":"parked","card":{"detail":{}}}]`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskDiagnoseCardsCmd
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

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no cards violate")) {
		t.Errorf("expected a clear no-violations message, got: %s", out.String())
	}
}

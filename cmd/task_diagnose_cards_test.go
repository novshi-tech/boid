package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

// TestTaskDiagnoseCards_ListsOnlyViolatingCards pins card-next-step-and-
// timeline.md §8's read-only diagnostic: a card with two open children is
// flagged, a healthy single-child card is not, and no state-changing
// request is ever made (the httptest server only ever serves GET).
func TestTaskDiagnoseCards_ListsOnlyViolatingCards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected non-GET request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
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

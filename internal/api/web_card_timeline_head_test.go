package api

// A child pinned when a card's first history page loaded, that closes
// before "Load older" is ever clicked, reappears in history at its
// ORIGINAL (pinned-era) creation position — which the client's
// already-established Load-older cursor has already skipped past, since
// that cursor was computed while the child was still excluded. No future
// "older than cursor" fetch will ever ask for that position again.
// TaskCardTimelineHead closes the gap: given the boundary cursor the client
// already holds, it re-renders the range down to (and including) that same
// boundary item, so a client can safely replace its own already-loaded
// head range in place.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCardDetail_TimelineHead_RevealsChildSkippedByStaleCursor(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	stamp := func(a *orchestrator.Action, minutes int) {
		t.Helper()
		if _, err := repo.Exec(`UPDATE actions SET created_at = ? WHERE id = ?`, base.Add(time.Duration(minutes)*time.Minute), a.ID); err != nil {
			t.Fatalf("stamp action %s: %v", a.ID, err)
		}
	}

	// The child is created (anchor action) at +50, while pinned (still
	// "open") it is excluded from every history computation.
	childAdded := createCardTimelineAction(t, repo, "card-1", "child_added", map[string]string{"id": "c1", "title": "the pinned child"})
	stamp(childAdded, 50)
	tt, err := orchestrator.GetTaskTriage(repo, "card-1")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.AddDetailChild(tt.Detail, orchestrator.TaskTriageChild{ID: "c1", Title: "the pinned child"})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(repo, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	// 9 notes strictly newer than the child (ranks 1-9 once the child is
	// excluded) at +51..+59.
	for i := 1; i <= 9; i++ {
		a := createCardTimelineAction(t, repo, "card-1", "attrs_set", map[string]string{"summary": "newer minute " + strconv.Itoa(50+i)})
		stamp(a, 50+i)
	}
	// 10 notes strictly older than the child at +40..+31 (newest of this
	// group, +40, is what a pinned-excluded page1 fetch computes as its
	// Load-older cursor — rank 10 among the 19 non-pinned items, but the
	// child's TRUE (unpinned) rank is also 10, one slot earlier).
	var older40 *orchestrator.Action
	for i := 0; i < 10; i++ {
		minute := 40 - i
		a := createCardTimelineAction(t, repo, "card-1", "attrs_set", map[string]string{"summary": "older minute " + strconv.Itoa(minute)})
		stamp(a, minute)
		if minute == 40 {
			older40 = a
		}
	}
	if older40 == nil {
		t.Fatal("older40 action not captured")
	}

	// Load page 1 (child still pinned/open, so excluded) and capture the
	// exact Load-older cursor the client would now hold.
	code, page1 := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200; body:\n%s", code, page1)
	}
	if !strings.Contains(page1, "the pinned child") {
		t.Fatalf("child should render as a pinned item on page1; body:\n%s", page1)
	}
	if strings.Count(page1, "older minute 40") != 1 {
		t.Fatalf("page1 should include the rank-10 non-pinned item exactly once; body:\n%s", page1)
	}
	frontier, lastDate := extractLoadOlderParams(t, page1)

	// The child closes (dropped) BEFORE any "Load older" click — its own
	// history item now reappears at its +50 creation position, which page1's
	// already-established frontier cursor has skipped past.
	tt, err = orchestrator.GetTaskTriage(repo, "card-1")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, changed, err := orchestrator.DropDetailChild(tt.Detail, "c1")
	if err != nil {
		t.Fatalf("DropDetailChild: %v", err)
	}
	if !changed {
		t.Fatalf("expected DropDetailChild to report changed=true")
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(repo, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}
	createCardTimelineAction(t, repo, "card-1", "child_dropped", map[string]string{"id": "c1"})

	// Confirm the gap actually exists first: a real "Load older" call using
	// the stale frontier must NOT surface the child (it is chronologically
	// newer than the frontier, so it never satisfies "older than cursor").
	olderPageURL := "/tasks/card-1/card-timeline?" + url.Values{"cursor": {frontier}, "last_date": {lastDate}}.Encode()
	_, olderPage := getHTML(t, h, olderPageURL)
	if strings.Contains(olderPage, "the pinned child") {
		t.Fatalf("test setup invalid: a real Load-older call must not see the child (gap not reproduced); body:\n%s", olderPage)
	}

	// TaskCardTimelineHead, given the exact frontier the client already
	// holds, must re-surface the child (and its Finished marker) while
	// still including the original boundary item exactly once and nothing
	// beyond it (no rewind of not-yet-loaded "Load older" territory).
	headURL := "/tasks/card-1/card-timeline/head?" + url.Values{"frontier": {frontier}}.Encode()
	code, head := getHTML(t, h, headURL)
	if code != http.StatusOK {
		t.Fatalf("head status = %d, want 200; body:\n%s", code, head)
	}
	if !strings.Contains(head, "the pinned child") {
		t.Errorf("head fragment should reveal the previously-skipped child; body:\n%s", head)
	}
	if !strings.Contains(head, "Finished:") {
		t.Errorf("head fragment should include the child's Finished marker; body:\n%s", head)
	}
	if got := strings.Count(head, "older minute 40"); got != 1 {
		t.Errorf("head fragment should include the original boundary item exactly once, got %d; body:\n%s", got, head)
	}
	if strings.Contains(head, "older minute 39") || strings.Contains(head, "older minute 31") {
		t.Errorf("head fragment must not reach past the frontier into not-yet-loaded territory; body:\n%s", head)
	}
	for i := 1; i <= 9; i++ {
		want := "newer minute " + strconv.Itoa(50+i)
		if strings.Count(head, want) != 1 {
			t.Errorf("head fragment should include %q exactly once; body:\n%s", want, head)
		}
	}
}

// TestCardDetail_TimelineHead_MalformedFrontier_BadRequest pins that a
// corrupted/foreign frontier cursor is a hard 400, the same posture
// DecodeActionCursor's own doc comment already commits to for every other
// cursor-consuming endpoint — silently treating it as "from the beginning"
// would return the entire history instead of surfacing the mistake.
func TestCardDetail_TimelineHead_MalformedFrontier_BadRequest(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body := getHTML(t, h, "/tasks/card-1/card-timeline/head?frontier=not-a-cursor")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", code, body)
	}
}

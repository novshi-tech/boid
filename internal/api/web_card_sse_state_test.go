package api

// #task-status and #task-pinned are both replaced wholesale (outerHTML) on
// every relevant SSE event, which would otherwise close an open <details>
// and jump the scroll position whenever the replacement's height differs.
// TaskDetailLiveScript's refresh() wraps every such swap with a
// capture/restore pair instead of adopting a diffing library.
//
// No headless browser is available in this repo's test toolchain, so these
// tests only pin the SOURCE-level wiring — the same structural-assertion
// idiom already used by TestCardDetail_LiveScript_RefreshesPinnedKind — not
// the actual runtime DOM/scroll behavior.

import (
	"net/http"
	"strings"
	"testing"
)

// lineContainingIsActive finds the line containing marker and reports
// whether that line is live JS — not commented out with "//" — guarding
// against a mutation that comments out a call site rather than deleting it
// (which a plain substring/count check cannot tell apart from the real
// thing).
func lineContainingIsActive(t *testing.T, body, marker string) bool {
	t.Helper()
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("marker %q not found in body:\n%s", marker, body)
	}
	lineStart := strings.LastIndex(body[:idx], "\n") + 1
	line := body[lineStart:idx]
	return !strings.Contains(line, "//")
}

// TestCardDetail_LiveScript_CapturesAndRestoresSwapState pins that refresh()
// captures state from the element being replaced BEFORE the outerHTML swap
// and restores it onto the freshly-swapped-in element AFTER — not just that
// the helper functions exist somewhere in the script.
func TestCardDetail_LiveScript_CapturesAndRestoresSwapState(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}

	captureIdx := strings.Index(body, "captureSwapState(before)")
	swapIdx := strings.Index(body, ".outerHTML = html")
	// The specific CALL site, not restoreSwapState's own definition (which
	// is declared earlier in the script, before refresh() ever runs).
	restoreIdx := strings.Index(body, "restoreSwapState(document.getElementById")
	if captureIdx < 0 || swapIdx < 0 || restoreIdx < 0 {
		t.Fatalf("expected captureSwapState/outerHTML swap/restoreSwapState all present in refresh(); body:\n%s", body)
	}
	if !(captureIdx < swapIdx && swapIdx < restoreIdx) {
		t.Errorf("expected capture-before-swap-before-restore ordering (got capture=%d swap=%d restore=%d); body:\n%s", captureIdx, swapIdx, restoreIdx, body)
	}
	if !lineContainingIsActive(t, body, "captureSwapState(before)") {
		t.Errorf("captureSwapState(before) call must not be commented out; body:\n%s", body)
	}
	if !lineContainingIsActive(t, body, "restoreSwapState(document.getElementById") {
		t.Errorf("restoreSwapState(...) call must not be commented out; body:\n%s", body)
	}
}

// TestCardDetail_LiveScript_RefreshHistoryHeadWiredToActionAndRevisit pins
// that the child-closed self-broadcast ('action' Kind, since its TaskID is
// already the parent card — see card_child_fanout.go) triggers
// refreshHistoryHead(), and that returning to the page (visibilitychange to
// visible, pageshow) re-syncs it too — not just the initial two SSE kinds.
// 'job' and 'child' events are deliberately excluded: neither ever moves an
// item out of #task-pinned into #card-timeline's history range (only a
// card's own action — a child_closed self-broadcast — does).
func TestCardDetail_LiveScript_RefreshHistoryHeadWiredToActionAndRevisit(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	_, body := getHTML(t, h, "/tasks/card-1")

	if got := strings.Count(body, "refreshHistoryHead()"); got != 4 {
		t.Errorf("refreshHistoryHead() should appear 4 times (1 definition + 3 call sites: action listener, visibilitychange, pageshow), got %d; body:\n%s", got, body)
	}

	actionListenerIdx := strings.Index(body, "addEventListener('action'")
	jobListenerIdx := strings.Index(body, "addEventListener('job'")
	childListenerIdx := strings.Index(body, "addEventListener('child'")
	if actionListenerIdx < 0 || jobListenerIdx < 0 || childListenerIdx < 0 {
		t.Fatalf("expected all three SSE listeners present; body:\n%s", body)
	}
	actionListenerEnd := strings.Index(body[actionListenerIdx:], "\n")
	if actionListenerEnd < 0 || !strings.Contains(body[actionListenerIdx:actionListenerIdx+actionListenerEnd], "refreshHistoryHead()") {
		t.Errorf("the 'action' listener's own line should call refreshHistoryHead(); body:\n%s", body)
	}
	if !lineContainingIsActive(t, body[actionListenerIdx:], "refreshHistoryHead()") {
		t.Errorf("the 'action' listener's refreshHistoryHead() call must not be commented out; body:\n%s", body)
	}
	jobListenerEnd := strings.Index(body[jobListenerIdx:], "\n")
	if jobListenerEnd >= 0 && strings.Contains(body[jobListenerIdx:jobListenerIdx+jobListenerEnd], "refreshHistoryHead()") {
		t.Errorf("the 'job' listener must NOT call refreshHistoryHead() (jobs never move a pinned item into history); body:\n%s", body)
	}
	childListenerEnd := strings.Index(body[childListenerIdx:], "\n")
	if childListenerEnd >= 0 && strings.Contains(body[childListenerIdx:childListenerIdx+childListenerEnd], "refreshHistoryHead()") {
		t.Errorf("the 'child' listener must NOT call refreshHistoryHead() (child fan-out never moves a pinned item into history); body:\n%s", body)
	}
}

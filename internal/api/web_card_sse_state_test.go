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
	"fmt"
	"net/http"
	"regexp"
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

// History refresh follows card actions, child updates, and page revisits.
func TestCardDetail_LiveScript_RefreshHistoryHeadWiredToActionAndRevisit(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	_, body := getHTML(t, h, "/tasks/card-1")

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
	if jobListenerEnd >= 0 && !strings.Contains(body[jobListenerIdx:jobListenerIdx+jobListenerEnd], "refreshHistoryHead()") {
		t.Errorf("the 'job' listener must refresh operation associations in history; body:\n%s", body)
	}
	childListenerEnd := strings.Index(body[childListenerIdx:], "\n")
	if childListenerEnd >= 0 && !strings.Contains(body[childListenerIdx:childListenerIdx+childListenerEnd], "refreshHistoryHead()") {
		t.Errorf("the 'child' listener must refresh child state in history; body:\n%s", body)
	}
}

// TestCardDetail_LiveScript_HistoryHeadSelectorsMatchRenderedMarkup pins the
// seam between refreshHistoryHead()'s hardcoded JS selectors and the actual
// markup CardHistorySection/cardHistoryLoadOlder render — a one-token typo
// on either side (JS selector or template class/query-param name) makes
// refreshHistoryHead() a permanent silent no-op with no test noticing it
// unless the two sides are cross-checked directly.
func TestCardDetail_LiveScript_HistoryHeadSelectorsMatchRenderedMarkup(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	for i := 0; i < 12; i++ {
		createCardTimelineAction(t, repo, "card-1", "attrs_set", map[string]string{"summary": "note number " + fmt.Sprintf("%02d", i)})
	}

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}

	// The exact JS selectors refreshHistoryHead() uses to find the list and
	// its Load-older boundary.
	for _, want := range []string{
		"querySelector('.card-timeline-list')",
		"querySelector('.card-timeline-load-older button')",
		"searchParams.get('cursor')",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected refreshHistoryHead() to contain %q; body:\n%s", want, body)
		}
	}

	// The exact markup those selectors must match — 12 history items forces
	// a real Load-older control to render.
	if !strings.Contains(body, `class="card-timeline-list"`) {
		t.Fatalf("expected a rendered .card-timeline-list; body:\n%s", body)
	}
	if !strings.Contains(body, `class="card-timeline-load-older"`) {
		t.Fatalf("expected a rendered Load-older control with 12 history items past the 10-cap; body:\n%s", body)
	}
	loadOlderIdx := strings.Index(body, "card-timeline-load-older")
	hxGetIdx := strings.Index(body[loadOlderIdx:], "hx-get=")
	if hxGetIdx < 0 {
		t.Fatalf("expected an hx-get attribute on the Load-older control; body:\n%s", body)
	}
	hxGetRegion := body[loadOlderIdx+hxGetIdx : loadOlderIdx+hxGetIdx+200]
	if !strings.Contains(hxGetRegion, "cursor=") {
		t.Errorf("Load-older's hx-get should carry a cursor= param (the exact name refreshHistoryHead() reads via searchParams.get('cursor')); got: %s", hxGetRegion)
	}

	// The URL the script actually builds must be a route the server serves.
	// Asserting only that the literals exist lets a typo'd path or param
	// name turn refreshHistoryHead() into a permanent silent no-op (r.ok is
	// false, the handler returns early) with the whole suite still green.
	assertScriptHeadURLIsServed(t, h, body)
}

// assertScriptHeadURLIsServed extracts the head endpoint refreshHistoryHead()
// builds and issues it, so the JS literal and the registered route cannot
// drift apart.
func assertScriptHeadURLIsServed(t *testing.T, h *WebHandler, body string) {
	t.Helper()
	m := regexp.MustCompile(`return '/tasks/' \+ id \+ '([^']+)' \+ encodeURIComponent\(frontier\);`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("could not find historyHeadURL's path literal in the rendered script; body:\n%s", body)
	}
	// The param name too: the server treats an unknown one as an empty
	// frontier and still answers 200, so reaching the route is not enough —
	// the client would splice the whole history in above a mid-list cursor.
	if !strings.HasSuffix(m[1], "?frontier=") {
		t.Errorf("script builds %q, want the head path ending in %q (TaskCardTimelineHead reads the frontier param by that name)", m[1], "?frontier=")
	}
	code, got := getHTML(t, h, "/tasks/card-1"+m[1])
	if code != http.StatusOK {
		t.Errorf("the script fetches /tasks/{id}%s, which the server answers with %d — refreshHistoryHead() would silently no-op; body:\n%s", m[1], code, got)
	}
}

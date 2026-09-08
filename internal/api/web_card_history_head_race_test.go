package api

// TaskDetailLiveScript's JS has no unit test runner wired into this repo,
// but the specific bug this test guards — a stale DOM reference thrown
// across an actual async race — cannot be pinned by any string/structural
// assertion (see web_card_sse_state_test.go's own note on that limit): it
// only manifests once refreshHistoryHead()'s real fetch-then-splice logic
// actually executes concurrently with a DOM mutation. This test extracts
// the CURRENT rendered script (so a source mutation is exercised
// automatically, no separate copy to keep in sync) and runs it for real
// under Node against a hand-built fake DOM
// (internal/api/testdata/history_head_race_harness.mjs).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractHistoryHeadJS pulls the captureSwapState/restoreSwapState/
// refresh/refreshHistoryHead function block out of a rendered
// TaskDetailLiveScript body — the same four functions, in source order,
// with no re-typing that could drift from the real script.
func extractHistoryHeadJS(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "function captureSwapState(el) {")
	if start < 0 {
		t.Fatalf("captureSwapState not found in body:\n%s", body)
	}
	end := strings.Index(body, "function openES() {")
	if end < 0 || end <= start {
		t.Fatalf("openES (end marker) not found after captureSwapState in body:\n%s", body)
	}
	return body[start:end]
}

// TestCardDetail_LiveScript_RefreshHistoryHead_RealExecution runs
// refreshHistoryHead() for real (under Node) against two scenarios the
// harness drives in sequence: (1) an HTMX "Load older" swap detaching the
// splice boundary while a fetch is still in flight must not empty the
// already-loaded list or throw on a stale insertBefore reference, and (2) a
// history item's own open <details> must re-open after the splice, the
// same way refresh() already does for #task-status/#task-pinned.
func TestCardDetail_LiveScript_RefreshHistoryHead_RealExecution(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available on PATH; skipping real-execution JS race test")
	}

	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	_, body := getHTML(t, h, "/tasks/card-1")
	chunk := extractHistoryHeadJS(t, body)

	dir := t.TempDir()
	chunkPath := filepath.Join(dir, "history_head_chunk.js")
	if err := os.WriteFile(chunkPath, []byte(chunk), 0o600); err != nil {
		t.Fatalf("write extracted JS chunk: %v", err)
	}

	harness, err := filepath.Abs("testdata/history_head_js_harness.mjs")
	if err != nil {
		t.Fatalf("resolve harness path: %v", err)
	}
	cmd := exec.Command(nodeBin, harness, chunkPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("history head JS harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("expected harness to report OK; got:\n%s", out)
	}
}

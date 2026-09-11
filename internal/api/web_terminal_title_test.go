package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobTerminalTitleRendering(t *testing.T) {
	svc := &stubWebService{jobDetail: &JobWithContext{Job: Job{
		ID: "title-job", Role: "session", Status: JobStatusRunning,
		DisplayName: "claude session", DisplayNameDefault: true,
		TerminalTitle: "日本語 <script>alert(1)</script>",
	}}}
	r := newTestWebHandlerWithJobDetail(svc)
	for _, fragment := range []bool{false, true} {
		url := "/jobs/title-job"
		if fragment {
			url += "?title=1"
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "日本語 &lt;script&gt;") || strings.Contains(body, "<script>alert(1)</script>") {
			t.Fatalf("unexpected title response: %s", body)
		}
		if !strings.Contains(body, "every 2s") {
			t.Fatal("running title must refresh")
		}
		if fragment && (strings.Contains(body, "<html") || strings.Contains(body, "boid-terminal")) {
			t.Fatal("title refresh must not recreate terminal")
		}
	}
	svc.jobDetail.Status = JobStatusCompleted
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jobs/title-job?title=1", nil))
	if strings.Contains(w.Body.String(), "every 2s") {
		t.Fatal("finished title must stop polling")
	}
}

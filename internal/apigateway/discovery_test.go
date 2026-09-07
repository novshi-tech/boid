package apigateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The discovery route answers the one question a sandboxed caller cannot
// answer for itself: which service names its own job token may reach.

func decodeDiscovery(t *testing.T, body []byte) discoveryResponse {
	t.Helper()
	var got discoveryResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal discovery response %q: %v", body, err)
	}
	return got
}

func TestServer_Discovery_ListsTheTokensOwnServices(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"myapp", "otherapp"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://example.invalid"},
		{Name: "otherapp", BaseURL: "https://example.invalid"},
	}, stubResolver(nil))

	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/"+token, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	got := decodeDiscovery(t, w.Body.Bytes())
	if len(got.Services) != 2 {
		t.Fatalf("services = %+v, want 2 entries", got.Services)
	}
	// Sorted, so a caller diffing two responses sees a real change rather
	// than map iteration order.
	if got.Services[0].Name != "myapp" || got.Services[1].Name != "otherapp" {
		t.Errorf("services = %+v, want [myapp otherapp] in that order", got.Services)
	}
	if got.ReadOnly {
		t.Error("readonly = true, want false for a token registered with readOnly=false")
	}

	if rec.count() != 0 {
		t.Errorf("recorder was called %d times, want 0 — discovery reaches no upstream and resolves no credential", rec.count())
	}
}

func TestServer_Discovery_TrailingSlashIsTheSameRoute(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	srv := NewServer(registry, NewCredentialProvider(nil, stubResolver(nil)), nil, nil)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/"+token+"/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if got := decodeDiscovery(t, w.Body.Bytes()); len(got.Services) != 1 || got.Services[0].Name != "myapp" {
		t.Errorf("services = %+v, want [myapp]", got.Services)
	}
}

func TestServer_Discovery_ReportsReadOnly(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", true)
	srv := NewServer(registry, NewCredentialProvider(nil, stubResolver(nil)), nil, nil)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/"+token, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if !decodeDiscovery(t, w.Body.Bytes()).ReadOnly {
		t.Error("readonly = false, want true — a read-only job must learn that writes are refused before it attempts one")
	}
}

func TestServer_Discovery_ReportsRequiresAccount(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"plain", "qualified"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "plain", BaseURL: "https://example.invalid"},
		{Name: "qualified", BaseURL: "https://example.invalid", RequireAccount: true},
	}, stubResolver(nil))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/"+token, nil))

	got := decodeDiscovery(t, w.Body.Bytes())
	byName := map[string]discoveryService{}
	for _, s := range got.Services {
		byName[s.Name] = s
	}
	if byName["plain"].RequiresAccount {
		t.Error("plain.requires_account = true, want false")
	}
	if !byName["qualified"].RequiresAccount {
		t.Error("qualified.requires_account = false, want true — an account-less request to it is a 400, which the caller should be able to avoid")
	}
}

func TestServer_Discovery_UnknownTokenIsUnauthorized(t *testing.T) {
	registry := NewRegistry()
	registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	srv := NewServer(registry, NewCredentialProvider(nil, stubResolver(nil)), nil, nil)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/not-a-real-token", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if body := w.Body.String(); jsonLooking(body) {
		t.Errorf("body = %q, want a plain-text error that names no service", body)
	}
}

func TestServer_Discovery_RejectsNonSafeMethods(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	srv := NewServer(registry, NewCredentialProvider(nil, stubResolver(nil)), nil, nil)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("POST", "/api/"+token, nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 — discovery is a read, and a POST here is a caller bug worth naming", w.Code)
	}
}

// jsonLooking reports whether body could be mistaken for the discovery
// document, so the 401 test can assert the error path leaks no service list.
func jsonLooking(body string) bool {
	var v any
	return json.Unmarshal([]byte(body), &v) == nil
}

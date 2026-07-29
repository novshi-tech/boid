package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeSecretStore is a minimal in-memory SecretStore for handler-level
// tests — it does not exercise encryption/persistence, only the
// namespace/key/value contract SecretHandler depends on.
type fakeSecretStore struct {
	values map[string]map[string]string // namespace -> key -> value
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{values: map[string]map[string]string{}}
}

func (f *fakeSecretStore) List(namespace string) ([]string, error) {
	var keys []string
	for k := range f.values[namespace] {
		keys = append(keys, k)
	}
	return keys, nil
}

func (f *fakeSecretStore) Set(namespace, key, value string) error {
	if f.values[namespace] == nil {
		f.values[namespace] = map[string]string{}
	}
	f.values[namespace][key] = value
	return nil
}

func (f *fakeSecretStore) Delete(namespace, key string) error {
	delete(f.values[namespace], key)
	return nil
}

func (f *fakeSecretStore) Get(namespace, key string) (string, error) {
	v, ok := f.values[namespace][key]
	if !ok {
		return "", fmt.Errorf("secret %q/%q: not found", namespace, key)
	}
	return v, nil
}

// TestSecretHandler_KeyWithSlash_GetAndDeleteRoundTrip pins the contract
// atl-cli's BoidCLIStore (and any other CredentialStore-shaped caller)
// relies on: a key containing a "/" — e.g. "<site-alias>/api-token", the
// exact shape internal/auth.SaveSite in novshi-tech/atl-cli uses — must
// round-trip through Set (JSON body, cmd/secret.go's runSecretSet) and then
// GetValue/Delete (URL path parameter, url.PathEscape'd by
// runSecretGet/runSecretDelete) unchanged.
//
// Set decodes `key` from the JSON body, so it never sees the escaped form.
// GetValue/Delete read `key` via chi.URLParam, which — since chi matches
// routes against the request's escaped path — hands the handler the
// STILL-ESCAPED "verify-test%2Fapi-token", not the unescaped
// "verify-test/api-token" Set stored it under. Without an explicit
// url.PathUnescape, GetValue/Delete look up (and, for Delete, silently
// no-op DELETE against) a key that was never stored, while Set's own key
// stays orphaned in the store — exactly what a boid-cli-backend atl saw in
// production dogfood (2026-07-29): `boid secret get <site>/api-token`
// returned "sql: no rows in result set" for a key `secret set` had just
// reported as saved.
func TestSecretHandler_KeyWithSlash_GetAndDeleteRoundTrip(t *testing.T) {
	store := newFakeSecretStore()
	h := &SecretHandler{Store: store}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	const namespace = "atl"
	const key = "verify-test/api-token"
	const value = "dummy-token-123"

	// Set: JSON body, key never touches URL encoding.
	setBody, err := json.Marshal(secretSetRequest{Namespace: namespace, Key: key, Value: value})
	if err != nil {
		t.Fatalf("marshal set request: %v", err)
	}
	resp, err := http.Post(srv.URL+"/", "application/json", bytes.NewReader(setBody))
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200", resp.StatusCode)
	}

	escapedKey := url.PathEscape(key)

	// GetValue: URL path parameter, matches runSecretGet's own url.PathEscape.
	getResp, err := http.Get(srv.URL + "/" + escapedKey + "/value?namespace=" + namespace)
	if err != nil {
		t.Fatalf("GET .../value: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/value status = %d, want 200 (key set via JSON body must be readable back via the escaped URL path form runSecretGet sends)", escapedKey, getResp.StatusCode)
	}
	var getOut struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&getOut); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getOut.Value != value {
		t.Errorf("GET value = %q, want %q", getOut.Value, value)
	}

	// Delete: same escaped-path shape as runSecretDelete.
	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/"+escapedKey+"?namespace="+namespace, nil)
	if err != nil {
		t.Fatalf("build DELETE request: %v", err)
	}
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE %s: %v", escapedKey, err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE %s status = %d, want 200", escapedKey, delResp.StatusCode)
	}

	keys, err := store.List(namespace)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, k := range keys {
		if k == key {
			t.Errorf("List still contains %q after DELETE — the delete matched a different (escaped-form) key than Set actually stored", key)
		}
	}
}

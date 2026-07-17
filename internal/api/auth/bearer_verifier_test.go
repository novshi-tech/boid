package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGenerateDeviceToken_HasPrefixAndIsUnique(t *testing.T) {
	tok1, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	tok2, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if tok1 == tok2 {
		t.Fatal("two generated tokens are equal, want unique")
	}
	for _, tok := range []string{tok1, tok2} {
		if len(tok) <= len(DeviceTokenPrefix) {
			t.Fatalf("token %q too short", tok)
		}
		if tok[:len(DeviceTokenPrefix)] != DeviceTokenPrefix {
			t.Errorf("token %q missing prefix %q", tok, DeviceTokenPrefix)
		}
	}
}

func TestHashToken_DeterministicAndDistinct(t *testing.T) {
	h1 := HashToken("boid_pat_abc")
	h2 := HashToken("boid_pat_abc")
	h3 := HashToken("boid_pat_xyz")
	if string(h1) != string(h2) {
		t.Error("HashToken is not deterministic for the same input")
	}
	if string(h1) == string(h3) {
		t.Error("HashToken produced the same hash for different inputs")
	}
}

func TestExtractBearerToken(t *testing.T) {
	cases := []struct {
		name        string
		header      string
		wantToken   string
		wantPresent bool
		wantOK      bool
	}{
		{"canonical", "Bearer boid_pat_xxx", "boid_pat_xxx", true, true},
		{"lowercase scheme is still Bearer", "bearer boid_pat_xxx", "boid_pat_xxx", true, true},
		{"uppercase scheme is still Bearer", "BEARER boid_pat_xxx", "boid_pat_xxx", true, true},
		{"mixed-case scheme is still Bearer", "BeArEr boid_pat_xxx", "boid_pat_xxx", true, true},
		{"multiple spaces between scheme and token are trimmed", "Bearer    boid_pat_xxx", "boid_pat_xxx", true, true},
		{"tab between scheme and token is trimmed", "Bearer\tboid_pat_xxx", "boid_pat_xxx", true, true},
		{"missing header — not present", "", "", false, false},
		{"wrong scheme — not present", "Basic dXNlcjpwYXNz", "", false, false},
		{"cookie-shaped value — not present", "boid_session=abc", "", false, false},
		// Malformed Bearer variants: present == true, ok == false. The
		// three-way return is exactly for this — callers must NOT fall
		// back to cookie auth when a Bearer header is present-but-bad.
		{"scheme-only (no space, no token) — present but not ok", "Bearer", "", true, false},
		{"empty token after prefix — present but not ok", "Bearer ", "", true, false},
		{"only whitespace after prefix — present but not ok", "Bearer    ", "", true, false},
		{"only tab after prefix — present but not ok", "Bearer\t\t", "", true, false},
	}
	// Ordinary single-value cases share a single request; a follow-up
	// sub-test below covers multiple Authorization values, which cannot
	// be expressed by simply Set()-ing one string.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			tok, present, ok := ExtractBearerToken(req)
			if tok != tc.wantToken || present != tc.wantPresent || ok != tc.wantOK {
				t.Errorf("ExtractBearerToken(%q) = (%q, present=%v, ok=%v), want (%q, present=%v, ok=%v)",
					tc.header, tok, present, ok, tc.wantToken, tc.wantPresent, tc.wantOK)
			}
		})
	}
}

func TestExtractBearerToken_MultipleAuthorizationHeaders_HardFails(t *testing.T) {
	// A request that sneaks a Basic value in front of a valid Bearer
	// value must not be able to fall through to cookie auth via the
	// Bearer scheme check reading only the FIRST Authorization header.
	// The middleware treats this as present-but-invalid Bearer → 401.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Add("Authorization", "Basic dXNlcjpwYXNz")
	req.Header.Add("Authorization", "Bearer boid_pat_xxx")
	tok, present, ok := ExtractBearerToken(req)
	if tok != "" || !present || ok {
		t.Errorf("multi-value Authorization: got (%q, present=%v, ok=%v), want (\"\", present=true, ok=false)", tok, present, ok)
	}
}

func TestBearerVerifier_ValidToken_ReturnsDeviceID(t *testing.T) {
	store := newTestStore(t)
	token, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(context.Background(), "dev-cli", "laptop", HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	v := NewBearerVerifier(store)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	deviceID, err := v.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if deviceID != "dev-cli" {
		t.Errorf("deviceID = %q, want %q", deviceID, "dev-cli")
	}
}

func TestBearerVerifier_ValidToken_UpdatesLastSeen(t *testing.T) {
	store := newTestStore(t)
	token, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(context.Background(), "dev-lastseen", "", HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}
	before, err := store.GetDeviceByTokenHash(context.Background(), HashToken(token))
	if err != nil || before == nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	v := NewBearerVerifier(store)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if _, err := v.Verify(req); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	after, err := store.GetDeviceByTokenHash(context.Background(), HashToken(token))
	if err != nil || after == nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Errorf("LastSeenAt did not advance: before=%v after=%v", before.LastSeenAt, after.LastSeenAt)
	}
}

func TestBearerVerifier_NoHeader_ErrInvalidSession(t *testing.T) {
	store := newTestStore(t)
	v := NewBearerVerifier(store)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)

	if _, err := v.Verify(req); err != ErrInvalidSession {
		t.Errorf("Verify with no header: got %v, want ErrInvalidSession", err)
	}
}

func TestBearerVerifier_UnknownToken_ErrInvalidSession(t *testing.T) {
	store := newTestStore(t)
	v := NewBearerVerifier(store)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer boid_pat_does-not-exist")

	if _, err := v.Verify(req); err != ErrInvalidSession {
		t.Errorf("Verify with unknown token: got %v, want ErrInvalidSession", err)
	}
}

func TestBearerVerifier_RevokedDevice_ErrInvalidSession(t *testing.T) {
	store := newTestStore(t)
	token, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(context.Background(), "dev-revoked", "", HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}
	if err := store.RevokeDevice(context.Background(), "dev-revoked"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	v := NewBearerVerifier(store)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := v.Verify(req); err != ErrInvalidSession {
		t.Errorf("Verify with revoked device: got %v, want ErrInvalidSession", err)
	}
}

func TestBearerVerifier_CookieOnlyDevice_TokenNeverMatches(t *testing.T) {
	// A device that only ever paired via cookie (InsertDevice, not
	// InsertDeviceToken) must never be reachable through the Bearer path —
	// its token_hash column is NULL, and no raw token could ever hash to
	// NULL.
	store := newTestStore(t)
	if err := store.InsertDevice(context.Background(), "dev-cookie-only", "browser", []byte("cookiehash")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	v := NewBearerVerifier(store)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	// Deliberately probe with the raw bytes that happen to be the cookie
	// hash's pre-image guess — must not match since cookie devices never set
	// token_hash at all.
	req.Header.Set("Authorization", "Bearer cookiehash")

	if _, err := v.Verify(req); err != ErrInvalidSession {
		t.Errorf("Verify against cookie-only device: got %v, want ErrInvalidSession", err)
	}
}

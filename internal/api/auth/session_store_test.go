package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

func newTestSigner(t *testing.T) (*SessionSigner, *Store) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewStore(d.Conn)
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return NewSessionSigner(secret, store), store
}

func requestWithCookies(w *httptest.ResponseRecorder) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func TestSessionSigner_RoundTrip(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-1", "laptop", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	w := httptest.NewRecorder()
	if err := signer.Issue(w, "dev-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := requestWithCookies(w)
	deviceID, err := signer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if deviceID != "dev-1" {
		t.Errorf("deviceID = %q, want %q", deviceID, "dev-1")
	}
}

func TestSessionSigner_ForgedHMAC(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-2", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  cookieName,
		Value: "dev-2." + "0000000000000000000000000000000000000000000000000000000000000000",
	})

	_, err := signer.Verify(req)
	if err != ErrInvalidSession {
		t.Errorf("Verify forged: got %v, want ErrInvalidSession", err)
	}
}

func TestSessionSigner_RevokedDevice(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-3", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	w := httptest.NewRecorder()
	if err := signer.Issue(w, "dev-3"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := store.RevokeDevice(ctx, "dev-3"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	req := requestWithCookies(w)
	_, err := signer.Verify(req)
	if err != ErrInvalidSession {
		t.Errorf("Verify revoked: got %v, want ErrInvalidSession", err)
	}
}

func TestSessionSigner_ForgedDeviceID(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-real", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	w := httptest.NewRecorder()
	if err := signer.Issue(w, "dev-real"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Replace deviceID with a non-existent one while keeping the original sig.
	original := w.Result().Cookies()[0].Value
	idx := strings.LastIndex(original, ".")
	tampered := "dev-fake" + original[idx:]

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: tampered})

	_, err := signer.Verify(req)
	if err != ErrInvalidSession {
		t.Errorf("Verify forged deviceID: got %v, want ErrInvalidSession", err)
	}
}

func TestSessionSigner_NoEpochHourExpiry(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-5", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	w := httptest.NewRecorder()
	if err := signer.Issue(w, "dev-5"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Verify twice in a row to confirm there is no time-based expiry in the HMAC.
	for i := range 2 {
		req := requestWithCookies(w)
		if _, err := signer.Verify(req); err != nil {
			t.Fatalf("Verify call %d: %v", i+1, err)
		}
	}
}

func TestSessionSigner_UpdatesLastSeen(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-4", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	before, err := store.GetDevice(ctx, "dev-4")
	if err != nil {
		t.Fatalf("GetDevice before: %v", err)
	}
	beforeLastSeen := before.LastSeenAt

	time.Sleep(10 * time.Millisecond)

	w := httptest.NewRecorder()
	if err := signer.Issue(w, "dev-4"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	req := requestWithCookies(w)
	if _, err := signer.Verify(req); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	after, err := store.GetDevice(ctx, "dev-4")
	if err != nil {
		t.Fatalf("GetDevice after: %v", err)
	}
	if !after.LastSeenAt.After(beforeLastSeen) {
		t.Errorf("LastSeenAt not updated: before=%v, after=%v", beforeLastSeen, after.LastSeenAt)
	}
}

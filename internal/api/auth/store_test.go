package auth

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(d.Conn)
}

func TestInsertAndConsumePairingCode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	hash := []byte("testhash")
	expiresAt := time.Now().Add(5 * time.Minute)

	if err := s.InsertPairingCode(ctx, hash, "my-device", expiresAt); err != nil {
		t.Fatalf("InsertPairingCode: %v", err)
	}

	label, err := s.ConsumePairingCode(ctx, hash)
	if err != nil {
		t.Fatalf("ConsumePairingCode: %v", err)
	}
	if label != "my-device" {
		t.Errorf("label = %q, want %q", label, "my-device")
	}
}

func TestConsumePairingCode_AlreadyConsumed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	hash := []byte("hash2")
	if err := s.InsertPairingCode(ctx, hash, "", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("InsertPairingCode: %v", err)
	}
	if _, err := s.ConsumePairingCode(ctx, hash); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	_, err := s.ConsumePairingCode(ctx, hash)
	if err != ErrCodeConsumed {
		t.Errorf("second consume: got %v, want ErrCodeConsumed", err)
	}
}

func TestConsumePairingCode_Expired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	hash := []byte("hash3")
	if err := s.InsertPairingCode(ctx, hash, "", time.Now().Add(-1*time.Second)); err != nil {
		t.Fatalf("InsertPairingCode: %v", err)
	}
	_, err := s.ConsumePairingCode(ctx, hash)
	if err != ErrCodeExpired {
		t.Errorf("got %v, want ErrCodeExpired", err)
	}
}

func TestConsumePairingCode_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.ConsumePairingCode(ctx, []byte("nonexistent"))
	if err != ErrCodeNotFound {
		t.Errorf("got %v, want ErrCodeNotFound", err)
	}
}

func TestInsertAndGetDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertDevice(ctx, "dev-1", "laptop", []byte("cookiehash")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	d, err := s.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d == nil {
		t.Fatal("GetDevice: got nil, want device")
	}
	if d.ID != "dev-1" {
		t.Errorf("ID = %q, want %q", d.ID, "dev-1")
	}
	if d.Label != "laptop" {
		t.Errorf("Label = %q, want %q", d.Label, "laptop")
	}
	if string(d.CookieHash) != "cookiehash" {
		t.Errorf("CookieHash = %q, want %q", d.CookieHash, "cookiehash")
	}
}

func TestGetDevice_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d, err := s.GetDevice(ctx, "no-such")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d != nil {
		t.Errorf("GetDevice: got %+v, want nil", d)
	}
}

func TestGetDevice_RevokedReturnsNil(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertDevice(ctx, "dev-r", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	if err := s.RevokeDevice(ctx, "dev-r"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	d, err := s.GetDevice(ctx, "dev-r")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d != nil {
		t.Errorf("GetDevice after revoke: got %+v, want nil", d)
	}
}

func TestRevokeDevice_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RevokeDevice(ctx, "no-such-id"); err != ErrDeviceNotFound {
		t.Errorf("RevokeDevice on missing id: got %v, want ErrDeviceNotFound", err)
	}
}

func TestRevokeDevice_AlreadyRevokedIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertDevice(ctx, "dev-x", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	if err := s.RevokeDevice(ctx, "dev-x"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	// Second revoke on the same id must succeed as a no-op (device exists, just already revoked).
	if err := s.RevokeDevice(ctx, "dev-x"); err != nil {
		t.Errorf("second revoke: got %v, want nil (idempotent)", err)
	}
}

func TestUpdateDeviceLastSeen(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertDevice(ctx, "dev-2", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	newTime := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	if err := s.UpdateDeviceLastSeen(ctx, "dev-2", newTime); err != nil {
		t.Fatalf("UpdateDeviceLastSeen: %v", err)
	}

	d, err := s.GetDevice(ctx, "dev-2")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d == nil {
		t.Fatal("GetDevice: got nil")
	}
	if !d.LastSeenAt.Equal(newTime) {
		t.Errorf("LastSeenAt = %v, want %v", d.LastSeenAt, newTime)
	}
}

func TestListDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertDevice(ctx, "a", "A", []byte("h1")); err != nil {
		t.Fatalf("InsertDevice a: %v", err)
	}
	if err := s.InsertDevice(ctx, "b", "B", []byte("h2")); err != nil {
		t.Fatalf("InsertDevice b: %v", err)
	}
	if err := s.RevokeDevice(ctx, "b"); err != nil {
		t.Fatalf("RevokeDevice b: %v", err)
	}

	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("ListDevices: got %d devices, want 2", len(devices))
	}

	// device b should be revoked
	var foundB bool
	for _, d := range devices {
		if d.ID == "b" {
			foundB = true
			if d.RevokedAt == nil {
				t.Error("device b: RevokedAt should not be nil")
			}
		}
	}
	if !foundB {
		t.Error("device b not found in list")
	}
}

func TestRevokeAllDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"x", "y", "z"} {
		if err := s.InsertDevice(ctx, id, "", []byte("h")); err != nil {
			t.Fatalf("InsertDevice %s: %v", id, err)
		}
	}

	if err := s.RevokeAllDevices(ctx); err != nil {
		t.Fatalf("RevokeAllDevices: %v", err)
	}

	has, err := s.HasAnyDevice(ctx)
	if err != nil {
		t.Fatalf("HasAnyDevice: %v", err)
	}
	if has {
		t.Error("HasAnyDevice: got true after RevokeAllDevices, want false")
	}
}

func TestDeleteRevokedDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"keep", "revoke1", "revoke2"} {
		if err := s.InsertDevice(ctx, id, "", []byte("h")); err != nil {
			t.Fatalf("InsertDevice %s: %v", id, err)
		}
	}
	for _, id := range []string{"revoke1", "revoke2"} {
		if err := s.RevokeDevice(ctx, id); err != nil {
			t.Fatalf("RevokeDevice %s: %v", id, err)
		}
	}

	// dry run: count without deleting
	n, err := s.DeleteRevokedDevices(ctx, true)
	if err != nil {
		t.Fatalf("DeleteRevokedDevices dry_run: %v", err)
	}
	if n != 2 {
		t.Errorf("dry_run count = %d, want 2", n)
	}
	devices, _ := s.ListDevices(ctx)
	if len(devices) != 3 {
		t.Errorf("ListDevices after dry_run: got %d, want 3", len(devices))
	}

	// actual delete
	n, err = s.DeleteRevokedDevices(ctx, false)
	if err != nil {
		t.Fatalf("DeleteRevokedDevices: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted count = %d, want 2", n)
	}
	devices, _ = s.ListDevices(ctx)
	if len(devices) != 1 {
		t.Errorf("ListDevices after delete: got %d, want 1", len(devices))
	}
	if devices[0].ID != "keep" {
		t.Errorf("remaining device ID = %q, want %q", devices[0].ID, "keep")
	}
}

func TestInsertDeviceToken_GetDeviceByTokenHash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tokenHash := []byte("bearertokenhash")
	if err := s.InsertDeviceToken(ctx, "dev-bearer", "cli-laptop", tokenHash); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	d, err := s.GetDeviceByTokenHash(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if d == nil {
		t.Fatal("GetDeviceByTokenHash: got nil, want device")
	}
	if d.ID != "dev-bearer" {
		t.Errorf("ID = %q, want %q", d.ID, "dev-bearer")
	}
	if d.Label != "cli-laptop" {
		t.Errorf("Label = %q, want %q", d.Label, "cli-laptop")
	}
	// Bearer-only devices have no cookie.
	if d.CookieHash != nil {
		t.Errorf("CookieHash = %q, want nil", d.CookieHash)
	}
}

func TestGetDeviceByTokenHash_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d, err := s.GetDeviceByTokenHash(ctx, []byte("no-such-hash"))
	if err != nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if d != nil {
		t.Errorf("GetDeviceByTokenHash: got %+v, want nil", d)
	}
}

func TestGetDeviceByTokenHash_RevokedReturnsNil(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tokenHash := []byte("revoked-token-hash")
	if err := s.InsertDeviceToken(ctx, "dev-bearer-r", "", tokenHash); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}
	if err := s.RevokeDevice(ctx, "dev-bearer-r"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	d, err := s.GetDeviceByTokenHash(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if d != nil {
		t.Errorf("GetDeviceByTokenHash after revoke: got %+v, want nil", d)
	}
}

func TestInsertDeviceToken_DistinctFromCookieDevice(t *testing.T) {
	// A Bearer device and a cookie device can coexist in the store; looking
	// one up by its own hash must not return the other.
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertDevice(ctx, "dev-cookie", "browser", []byte("cookiehash")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	if err := s.InsertDeviceToken(ctx, "dev-bearer2", "cli", []byte("bearerhash2")); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	d, err := s.GetDeviceByTokenHash(ctx, []byte("cookiehash"))
	if err != nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if d != nil {
		t.Errorf("GetDeviceByTokenHash(cookiehash) = %+v, want nil (cookie device has no token_hash)", d)
	}

	got, err := s.GetDeviceByTokenHash(ctx, []byte("bearerhash2"))
	if err != nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if got == nil || got.ID != "dev-bearer2" {
		t.Errorf("GetDeviceByTokenHash(bearerhash2) = %+v, want dev-bearer2", got)
	}
}

func TestHasAnyDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	has, err := s.HasAnyDevice(ctx)
	if err != nil {
		t.Fatalf("HasAnyDevice: %v", err)
	}
	if has {
		t.Error("HasAnyDevice: got true on empty store, want false")
	}

	if err := s.InsertDevice(ctx, "dev", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	has, err = s.HasAnyDevice(ctx)
	if err != nil {
		t.Fatalf("HasAnyDevice: %v", err)
	}
	if !has {
		t.Error("HasAnyDevice: got false after insert, want true")
	}
}

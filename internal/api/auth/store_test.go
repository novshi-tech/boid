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

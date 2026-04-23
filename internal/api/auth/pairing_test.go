package auth

import (
	"context"
	"regexp"
	"testing"
)

var pairingCodePattern = regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`)

func TestGeneratePairingCode_Format(t *testing.T) {
	for range 20 {
		code := GeneratePairingCode()
		if !pairingCodePattern.MatchString(code) {
			t.Errorf("GeneratePairingCode() = %q, want format XXXX-XXXX", code)
		}
	}
}

func TestPairingManager_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	mgr := NewPairingManager(store)
	ctx := context.Background()

	code, err := mgr.Issue(ctx, "my-laptop")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !pairingCodePattern.MatchString(code) {
		t.Errorf("Issue returned code %q with invalid format", code)
	}

	label, err := mgr.Redeem(ctx, code)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if label != "my-laptop" {
		t.Errorf("Redeem label = %q, want %q", label, "my-laptop")
	}
}

func TestPairingManager_DoubleRedeem(t *testing.T) {
	store := newTestStore(t)
	mgr := NewPairingManager(store)
	ctx := context.Background()

	code, err := mgr.Issue(ctx, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := mgr.Redeem(ctx, code); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}

	_, err = mgr.Redeem(ctx, code)
	if err == nil {
		t.Error("second Redeem: expected error, got nil")
	}
}

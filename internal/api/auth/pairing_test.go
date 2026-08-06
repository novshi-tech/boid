package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
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

// TestGeneratePairingCode_NoConfusableChars pins the "human retypes the code
// from a terminal onto a phone" contract: 0/O and 1/I are indistinguishable in
// most terminal and mobile fonts, so a code containing them is a guaranteed
// support incident (the operator sees "無効なペアリングコードです" with no way
// to tell a typo from an expired code).
func TestGeneratePairingCode_NoConfusableChars(t *testing.T) {
	for range 200 {
		code := GeneratePairingCode()
		if strings.ContainsAny(code, "O0I1") {
			t.Fatalf("GeneratePairingCode() = %q, must not contain confusable chars O/0/I/1", code)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"canonical", "ABCD-EFGH", "ABCDEFGH"},
		{"lowercase", "abcd-efgh", "ABCDEFGH"},
		{"no hyphen", "ABCDEFGH", "ABCDEFGH"},
		{"surrounding space", "  ABCD-EFGH\n", "ABCDEFGH"},
		{"inner space", "ABCD EFGH", "ABCDEFGH"},
		{"fullwidth", "ＡＢＣＤ－ＥＦＧＨ", "ABCDEFGH"},
		{"fullwidth digits", "２３４５－６７８９", "23456789"},
		{"empty", "", ""},
		{"punctuation only", "----", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeCode(tt.in); got != tt.want {
				t.Errorf("NormalizeCode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPairingManager_RedeemAcceptsMangledInput is the regression test for the
// 2026-08-06 incident: a phone-entered code that differs from the printed one
// only in case, hyphenation, or stray whitespace was hashed verbatim and
// rejected. Issue and Redeem must agree on the same normalized form.
func TestPairingManager_RedeemAcceptsMangledInput(t *testing.T) {
	mangle := map[string]func(string) string{
		"lowercase":      strings.ToLower,
		"no hyphen":      func(s string) string { return strings.ReplaceAll(s, "-", "") },
		"trailing space": func(s string) string { return s + " " },
		"leading space":  func(s string) string { return " " + s },
	}
	for name, f := range mangle {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			mgr := NewPairingManager(store)
			ctx := context.Background()

			code, err := mgr.Issue(ctx, "phone")
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			label, err := mgr.Redeem(ctx, f(code))
			if err != nil {
				t.Fatalf("Redeem(%q): %v", f(code), err)
			}
			if label != "phone" {
				t.Errorf("label = %q, want phone", label)
			}
		})
	}
}

// A code that normalizes to something else must still be rejected — the
// normalizer must not be so eager that it makes wrong codes match.
func TestPairingManager_RedeemRejectsDifferentCode(t *testing.T) {
	store := newTestStore(t)
	mgr := NewPairingManager(store)
	ctx := context.Background()

	if _, err := mgr.Issue(ctx, ""); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := mgr.Redeem(ctx, "ZZZZ-ZZZZ"); !errors.Is(err, ErrCodeNotFound) {
		t.Errorf("Redeem(other code) error = %v, want ErrCodeNotFound", err)
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

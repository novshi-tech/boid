package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// pairingCodeChars deliberately omits O/0 and I/1: the code's whole job is to
// survive being read off a terminal and retyped on a phone, and those two
// pairs are indistinguishable in most fonts. Dropping them costs ~0.4 bits per
// character (36^8 → 32^8, still 2^40 of entropy behind a 5-attempt / 5-minute
// rate limiter) and removes the single most common retype failure.
const pairingCodeChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func GeneratePairingCode() string {
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(pairingCodeChars))))
		b[i] = pairingCodeChars[n.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// NormalizeCode reduces a user-entered pairing code to its canonical form:
// ASCII-uppercased alphanumerics only. Everything a human or a mobile keyboard
// adds on the way from the terminal to the login form — lowercase, the
// XXXX-XXXX hyphen, surrounding or inner whitespace, full-width characters
// from a Japanese IME — is dropped, so all of those redeem the same code.
//
// This exists because the code is matched by hash: without a shared
// normalization step on both the Issue and Redeem sides, a single stray space
// is indistinguishable from a wrong code, and the operator gets "invalid
// pairing code" for a code they typed correctly.
//
// It is deliberately lossy in one direction only: it never maps one legal code
// character onto another (no O→0 folding), so two distinct issued codes can
// never collide after normalization.
func NormalizeCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		// Fold full-width ASCII (U+FF01..U+FF5E) down to its ASCII
		// counterpart first — a Japanese IME left in full-width mode
		// produces these for every character, including the digits.
		if r >= '！' && r <= '～' {
			r = r - 0xFEE0
		}
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HashCode hashes the NORMALIZED code. Both Issue and Redeem go through here,
// which is what makes the normalization contract hold end to end.
func HashCode(code string) []byte {
	h := sha256.Sum256([]byte(NormalizeCode(code)))
	return h[:]
}

type PairingManager struct {
	store *Store
}

func NewPairingManager(store *Store) *PairingManager {
	return &PairingManager{store: store}
}

func (m *PairingManager) Issue(ctx context.Context, label string) (string, error) {
	code := GeneratePairingCode()
	hash := HashCode(code)
	expiresAt := time.Now().Add(5 * time.Minute)
	if err := m.store.InsertPairingCode(ctx, hash, label, expiresAt); err != nil {
		return "", fmt.Errorf("issue pairing code: %w", err)
	}
	return code, nil
}

func (m *PairingManager) Redeem(ctx context.Context, code string) (string, error) {
	hash := HashCode(code)
	label, err := m.store.ConsumePairingCode(ctx, hash)
	if err != nil {
		return "", fmt.Errorf("redeem pairing code: %w", err)
	}
	return label, nil
}

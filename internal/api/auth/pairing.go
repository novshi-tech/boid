package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"time"
)

const pairingCodeChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func GeneratePairingCode() string {
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(pairingCodeChars))))
		b[i] = pairingCodeChars[n.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func HashCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
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

// Package tunnel connects the daemon to a Heddleway gateway as a local
// service: pairing with the owner, keeping an outbound WebSocket open,
// serving the owner's requests and calling the gateway back as the owner.
// The wire protocol is Heddleway's docs/tunnel-protocol.md.
package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity is a paired local service: where it connects and the key that
// proves it. The file format matches Heddleway's tools/tunnel-client.mjs.
type Identity struct {
	Gateway    string
	ServiceID  string
	Name       string
	PrivateKey ed25519.PrivateKey
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	D   string `json:"d,omitempty"`
}

type identityFile struct {
	Gateway    string `json:"gateway"`
	ServiceID  string `json:"service_id"`
	Name       string `json:"name"`
	PrivateJWK jwk    `json:"private_jwk"`
}

var b64 = base64.RawURLEncoding

// GenerateKey creates a new Ed25519 key for pairing.
func GenerateKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

func publicJWK(priv ed25519.PrivateKey) jwk {
	return jwk{Kty: "OKP", Crv: "Ed25519", X: b64.EncodeToString(priv.Public().(ed25519.PublicKey))}
}

// Thumbprint returns the RFC 7638 thumbprint of the key's public half, the
// value the gateway shows the owner when approving.
func Thumbprint(priv ed25519.PrivateKey) string {
	pub := publicJWK(priv)
	canonical := fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q}`, pub.Crv, pub.Kty, pub.X)
	sum := sha256.Sum256([]byte(canonical))
	return b64.EncodeToString(sum[:])
}

// LoadIdentity reads an identity file. A missing file returns an error
// satisfying errors.Is(err, os.ErrNotExist).
func LoadIdentity(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("tunnel: parse %s: %w", path, err)
	}
	if f.PrivateJWK.Kty != "OKP" || f.PrivateJWK.Crv != "Ed25519" {
		return nil, fmt.Errorf("tunnel: %s does not hold an Ed25519 key", path)
	}
	seed, err := b64.DecodeString(f.PrivateJWK.D)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("tunnel: %s has a malformed private key", path)
	}
	if f.Gateway == "" || f.ServiceID == "" {
		return nil, fmt.Errorf("tunnel: %s is missing gateway or service_id", path)
	}
	return &Identity{
		Gateway:    strings.TrimRight(f.Gateway, "/"),
		ServiceID:  f.ServiceID,
		Name:       f.Name,
		PrivateKey: ed25519.NewKeyFromSeed(seed),
	}, nil
}

// SaveIdentity writes the identity file with mode 0600, replacing any
// previous one atomically.
func SaveIdentity(path string, id *Identity) error {
	pub := publicJWK(id.PrivateKey)
	pub.D = b64.EncodeToString(id.PrivateKey.Seed())
	raw, err := json.MarshalIndent(identityFile{Gateway: id.Gateway, ServiceID: id.ServiceID, Name: id.Name, PrivateJWK: pub}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tunnel-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// assertion signs the connect JWT for one challenge.
func (id *Identity) assertion(challenge string, now time.Time) string {
	header := b64.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss":   id.ServiceID,
		"aud":   id.Gateway + "/tunnel/connect",
		"nonce": challenge,
		"iat":   now.Unix(),
		"exp":   now.Add(60 * time.Second).Unix(),
	})
	input := header + "." + b64.EncodeToString(claims)
	return input + "." + b64.EncodeToString(ed25519.Sign(id.PrivateKey, []byte(input)))
}

var errNoIdentity = errors.New("tunnel: not paired")

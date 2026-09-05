package gitgateway

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateToken returns a random hex-encoded job token, using the same
// crypto/rand + 16-byte scheme as internal/sandbox's broker token registry.
func GenerateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("gitgateway: GenerateToken: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

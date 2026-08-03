package apigateway

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateToken returns a random hex-encoded job token, using the same
// crypto/rand + 16-byte scheme as internal/gitgateway.GenerateToken (and,
// beneath that, internal/sandbox's broker token registry). Duplicated rather
// than imported: apigateway is a leaf package, kept independent of
// internal/gitgateway on purpose so neither gateway package's tests
// (in particular a sandbox test run, which cannot build the sqlite-backed
// internal/db layers either package might otherwise be one import hop from)
// depend on the other's presence.
func GenerateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("apigateway: GenerateToken: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

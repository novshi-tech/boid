package apigateway

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateToken returns a random hex-encoded job token. Duplicated from
// internal/gitgateway.GenerateToken to keep this leaf package independent
// of it (see doc.go).
func GenerateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("apigateway: GenerateToken: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

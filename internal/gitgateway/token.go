package gitgateway

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateToken returns a random hex-encoded job token. It uses the same
// crypto/rand + 16-byte scheme as internal/sandbox's broker token registry
// (docs/plans/git-gateway-cutover.md PR3: 「job token は broker token と
// 同じ方式 (crypto/rand、後で PR4 で dispatch 時登録・job 終了時失効)」).
// PR3 only defines the registry API and token shape; the dispatch-time
// register/unregister lifecycle lands in PR4.
func GenerateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("gitgateway: GenerateToken: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

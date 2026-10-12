package gitgateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestVerifyReleaseUsesGatewayCredentials(t *testing.T) {
	commit := strings.Repeat("a", 40)
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "x-access-token" || pass != "secret" {
			t.Errorf("missing gateway authentication")
		}
		if r.URL.Query().Get("service") != "git-upload-pack" {
			t.Errorf("unexpected gateway operation: %s", r.URL)
		}
		if r.UserAgent() != "git/boid" {
			t.Errorf("User-Agent=%q", r.UserAgent())
		}
		line := commit + " refs/heads/main\x00multi_ack\n"
		fmt.Fprintf(w, "%04x%s0000", len(line)+4, line)
	})
	host := upstreamHost(t, upstream)
	registry := NewRegistry()
	credentials := NewCredentialProvider([]HostForgeConfig{{Host: host, Forge: ForgeGitHub, SecretKey: "pat", Scheme: "http"}}, func(namespace, key string) (string, error) {
		if namespace != "team" {
			t.Errorf("namespace=%q", namespace)
		}
		return "secret", nil
	})
	gateway := NewServer(registry, credentials, nil)
	key := NewRepoKey(host, "org", "repo")
	for _, tc := range []struct {
		branch, hash string
		want         bool
	}{{"main", commit, true}, {"origin/main", commit, true}, {"refs/heads/main", commit, true}, {"refs/remotes/origin/main", commit, true}, {"main", strings.Repeat("b", 40), false}, {"missing", commit, false}} {
		matches, err := gateway.VerifyRelease(context.Background(), key, "team", tc.branch, tc.hash)
		if err != nil || matches != tc.want {
			t.Errorf("%s %s: matches=%v err=%v", tc.branch, tc.hash, matches, err)
		}
	}
	if len(registry.entries) != 0 {
		t.Fatal("verification token leaked")
	}
}

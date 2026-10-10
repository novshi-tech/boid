package gitgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/checkout"
)

type fixtureTransport struct{ target *url.URL }

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Host = f.target.Host
	r.URL.Scheme = f.target.Scheme
	return http.DefaultTransport.RoundTrip(r)
}

func TestCheckoutPushPermissionsAndObservation(t *testing.T) {
	for _, writable := range []bool{false, true} {
		name := "readonly"
		if writable {
			name = "writable"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bare := newBareUpstreamRepo(t, root, "owner", "repo")
			seed := t.TempDir()
			runGit(t, seed, "init", "-b", "main")
			if err := os.WriteFile(filepath.Join(seed, "file"), []byte("seed"), 0600); err != nil {
				t.Fatal(err)
			}
			runGit(t, seed, "add", "file")
			runGit(t, seed, "commit", "-m", "seed")
			runGit(t, seed, "push", bare, "main")
			runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
			upstream := newGitHTTPBackendUpstream(t, root)
			target, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			repo := NewRepoKey("fixture.invalid", "owner", "repo")
			perm := PermFetch
			if writable {
				perm = PermFetchPush
			}
			reg := NewRegistry()
			token := reg.Register(map[RepoKey]Permission{repo: perm}, "default")
			var mu sync.Mutex
			var refs []string
			reg.ObservePush(token, func(_ RepoKey, ref string) { mu.Lock(); defer mu.Unlock(); refs = append(refs, ref) })
			gw := NewServer(reg, nil, nil)
			gw.proxy.Transport = fixtureTransport{target}
			srv := httptest.NewServer(gw)
			defer srv.Close()
			dir, err := checkout.Run(context.Background(), string(repo), srv.URL+"/j/"+token, t.TempDir(), t.TempDir(), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			runGit(t, dir, "switch", "-c", "boid/12345678")
			runGit(t, dir, "commit", "--allow-empty", "-m", "change")
			out, err := runGitAllowError(t, dir, "push", "origin", "HEAD:refs/heads/boid/12345678")
			if writable && err != nil {
				t.Fatalf("writable push: %v %s", err, out)
			}
			if !writable && (err == nil || !strings.Contains(out, "403")) {
				t.Fatalf("readonly push: %v %s", err, out)
			}
			mu.Lock()
			defer mu.Unlock()
			if writable && (len(refs) != 1 || refs[0] != "refs/heads/boid/12345678") {
				t.Fatalf("observed refs=%v", refs)
			}
			if !writable && len(refs) != 0 {
				t.Fatalf("denied push was observed as permitted: %v", refs)
			}
		})
	}
}

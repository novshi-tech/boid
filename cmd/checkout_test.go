package cmd

import "testing"

func TestCheckoutNamedRepo(t *testing.T) {
	for _, upstream := range []string{"https://github.com/owner/repo.git", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo.git", "ssh://git@github.com:2222/owner/repo.git"} {
		repo, err := checkoutNamedRepo("peer", []byte(`[{"name":"peer","upstream_url":"`+upstream+`"}]`))
		if err != nil || repo != "github.com/owner/repo" {
			t.Fatalf("%s: %q %v", upstream, repo, err)
		}
	}
	for _, data := range []string{`[]`, `[{"name":"peer"}]`, `[{"name":"peer","upstream_url":"https://github.com/owner/../repo"}]`, `[{"name":"peer"},{"name":"peer"}]`} {
		if _, err := checkoutNamedRepo("peer", []byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestCheckoutNamedRepoPreservesHTTPPort(t *testing.T) {
	repo, err := checkoutNamedRepo("peer", []byte(`[{"name":"peer","upstream_url":"https://host.docker.internal:42369/owner/repo.git"}]`))
	if err != nil || repo != "host.docker.internal:42369/owner/repo" {
		t.Fatalf("%q %v", repo, err)
	}
}

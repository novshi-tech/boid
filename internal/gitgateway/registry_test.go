package gitgateway

import "testing"

func TestRegistryAuthorize(t *testing.T) {
	repo := NewRepoKey("github.com", "owner", "repo")
	otherRepo := NewRepoKey("github.com", "owner", "other")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{
		repo: PermFetchPush,
	})

	t.Run("valid token, allowed repo, fetch", func(t *testing.T) {
		allowed, valid := reg.Authorize(token, repo, OpFetch)
		if !valid || !allowed {
			t.Fatalf("Authorize = (%v, %v), want (true, true)", allowed, valid)
		}
	})

	t.Run("valid token, allowed repo, push", func(t *testing.T) {
		allowed, valid := reg.Authorize(token, repo, OpPush)
		if !valid || !allowed {
			t.Fatalf("Authorize = (%v, %v), want (true, true)", allowed, valid)
		}
	})

	t.Run("valid token, repo not in allowed set", func(t *testing.T) {
		allowed, valid := reg.Authorize(token, otherRepo, OpFetch)
		if !valid {
			t.Fatalf("Authorize tokenValid = false, want true (token itself is valid)")
		}
		if allowed {
			t.Fatalf("Authorize allowed = true, want false (repo not registered)")
		}
	})

	t.Run("unknown token", func(t *testing.T) {
		allowed, valid := reg.Authorize("does-not-exist", repo, OpFetch)
		if valid || allowed {
			t.Fatalf("Authorize = (%v, %v), want (false, false)", allowed, valid)
		}
	})

	t.Run("unregister revokes", func(t *testing.T) {
		reg.Unregister(token)
		allowed, valid := reg.Authorize(token, repo, OpFetch)
		if valid || allowed {
			t.Fatalf("Authorize after Unregister = (%v, %v), want (false, false)", allowed, valid)
		}
	})
}

func TestRegistryFetchOnlyPermissionRejectsPush(t *testing.T) {
	repo := NewRepoKey("bitbucket.org", "team", "repo")
	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{repo: PermFetch})

	if allowed, valid := reg.Authorize(token, repo, OpFetch); !valid || !allowed {
		t.Fatalf("fetch should be allowed: (%v, %v)", allowed, valid)
	}
	if allowed, valid := reg.Authorize(token, repo, OpPush); !valid || allowed {
		t.Fatalf("push should be denied for fetch-only permission: (%v, %v)", allowed, valid)
	}
}

func TestRegistryRegisterTokenUsesExplicitToken(t *testing.T) {
	repo := NewRepoKey("github.com", "owner", "repo")
	reg := NewRegistry()
	reg.RegisterToken("explicit-token", map[RepoKey]Permission{repo: PermFetch})

	allowed, valid := reg.Authorize("explicit-token", repo, OpFetch)
	if !valid || !allowed {
		t.Fatalf("Authorize with explicit token = (%v, %v), want (true, true)", allowed, valid)
	}
}

func TestNilRegistryFailsClosed(t *testing.T) {
	var reg *Registry
	allowed, valid := reg.Authorize("anything", NewRepoKey("github.com", "o", "r"), OpFetch)
	if valid || allowed {
		t.Fatalf("nil Registry Authorize = (%v, %v), want (false, false)", allowed, valid)
	}
}

func TestPermissionAllows(t *testing.T) {
	if !PermFetch.Allows(OpFetch) {
		t.Fatal("PermFetch should allow fetch")
	}
	if PermFetch.Allows(OpPush) {
		t.Fatal("PermFetch should not allow push")
	}
	if !PermFetchPush.Allows(OpFetch) {
		t.Fatal("PermFetchPush should allow fetch")
	}
	if !PermFetchPush.Allows(OpPush) {
		t.Fatal("PermFetchPush should allow push")
	}
}

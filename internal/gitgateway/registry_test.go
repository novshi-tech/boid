package gitgateway

import "testing"

func TestRegistryAuthorize(t *testing.T) {
	repo := NewRepoKey("github.com", "owner", "repo")
	otherRepo := NewRepoKey("github.com", "owner", "other")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{
		repo: PermFetchPush,
	}, "ws-1")

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
	token := reg.Register(map[RepoKey]Permission{repo: PermFetch}, "default")

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
	reg.RegisterToken("explicit-token", map[RepoKey]Permission{repo: PermFetch}, "default")

	allowed, valid := reg.Authorize("explicit-token", repo, OpFetch)
	if !valid || !allowed {
		t.Fatalf("Authorize with explicit token = (%v, %v), want (true, true)", allowed, valid)
	}
}

// TestRegistryRegisterAndLookupPreserveNamespace is the guard for post-cutover
// 改善 §1 (workspace-scoped PAT namespace): Register must record the
// namespace it was given, and Lookup must return it back out unchanged, so
// Server.ServeHTTP can route CredentialProvider.Inject to the right
// workspace's secret.
func TestRegistryRegisterAndLookupPreserveNamespace(t *testing.T) {
	repo := NewRepoKey("github.com", "owner", "repo")
	reg := NewRegistry()

	tokenA := reg.Register(map[RepoKey]Permission{repo: PermFetch}, "ws-a")
	tokenB := reg.Register(map[RepoKey]Permission{repo: PermFetch}, "ws-b")

	entryA, ok := reg.Lookup(tokenA)
	if !ok {
		t.Fatal("Lookup(tokenA) not found")
	}
	if entryA.Namespace != "ws-a" {
		t.Errorf("entryA.Namespace = %q, want ws-a", entryA.Namespace)
	}

	entryB, ok := reg.Lookup(tokenB)
	if !ok {
		t.Fatal("Lookup(tokenB) not found")
	}
	if entryB.Namespace != "ws-b" {
		t.Errorf("entryB.Namespace = %q, want ws-b", entryB.Namespace)
	}
}

// TestRegistryRegisterEmptyNamespacePreserved pins the backward-compatibility
// fallback: a token registered with an empty namespace (workspace-unlinked
// project — orchestrator.JobSpec.SecretNamespace is "" in that case) must
// keep Namespace == "" through Register/Lookup; normalizing "" to "default"
// is dispatcher.SecretStore.Get's job (normalizeNamespace), not the
// Registry's.
func TestRegistryRegisterEmptyNamespacePreserved(t *testing.T) {
	repo := NewRepoKey("github.com", "owner", "repo")
	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{repo: PermFetch}, "")

	entry, ok := reg.Lookup(token)
	if !ok {
		t.Fatal("Lookup(token) not found")
	}
	if entry.Namespace != "" {
		t.Errorf("entry.Namespace = %q, want empty string preserved as-is", entry.Namespace)
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

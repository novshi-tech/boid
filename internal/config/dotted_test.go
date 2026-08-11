package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSet_Scalar(t *testing.T) {
	tree := Tree{}
	reload, err := Set(tree, "web.public_url", []string{"https://boid.example.com"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	// web.public_url is ReloadRestartRequired as of the PR #830 round-4
	// simplification (nose directive) — see ReloadDynamic's own doc comment.
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
	got, ok := GetPath(tree, "web.public_url")
	if !ok || got != "https://boid.example.com" {
		t.Errorf("GetPath = (%v, %v), want (https://boid.example.com, true)", got, ok)
	}
}

func TestSet_Array(t *testing.T) {
	tree := Tree{}
	_, err := Set(tree, "sandbox.allowed_domains", []string{".freee.co.jp", ".notion.com"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := GetPath(tree, "sandbox.allowed_domains")
	if !ok {
		t.Fatalf("GetPath: not found")
	}
	want := []any{".freee.co.jp", ".notion.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetPath = %#v, want %#v", got, want)
	}
}

func TestSet_MultiArgReplacesWholesale(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "sandbox.allowed_domains", []string{".a.com"}); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	if _, err := Set(tree, "sandbox.allowed_domains", []string{".b.com", ".c.com"}); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	got, _ := GetPath(tree, "sandbox.allowed_domains")
	want := []any{".b.com", ".c.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetPath = %#v, want %#v (wholesale replace, not append)", got, want)
	}
}

func TestSet_MapSegmentTraversal(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "gateway.forges.github.host", []string{"bitbucket.org"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := GetPath(tree, "gateway.forges.github.host")
	if !ok || got != "bitbucket.org" {
		t.Errorf("GetPath = (%v, %v), want (bitbucket.org, true)", got, ok)
	}
}

func TestSet_UnknownKeyRejected(t *testing.T) {
	tree := Tree{}
	_, err := Set(tree, "sandbox.alowed_domains", []string{"x"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("expected suggestion in error, got: %v", err)
	}
}

func TestSet_MapSlotWithoutLeafRejected(t *testing.T) {
	tree := Tree{}
	_, err := Set(tree, "gateway.forges.github", []string{"x"})
	if err == nil {
		t.Fatal("expected error: gateway.forges.github is a map slot, not a leaf")
	}
}

// TestSet_EnumValidation exercises KindEnum validation against
// gateway.forges.*.forge (github.com/bitbucket) — sandbox.backend used to be
// this test's example enum leaf, but PR-4 (docs/plans/volume-only-daemon.md
// §論点e) demoted it to KindOpaque (container is the only backend now); see
// TestSet_KindOpaque_SandboxBackend_Rejected below for its own coverage.
func TestSet_EnumValidation(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "gateway.forges.github.forge", []string{"bogus"}); err == nil {
		t.Fatal("expected error for invalid enum value")
	}
	reload, err := Set(tree, "gateway.forges.github.forge", []string{"github"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
}

// TestSet_KindOpaque_SandboxBackend_Rejected pins PR-4's removal of
// sandbox.backend (docs/plans/volume-only-daemon.md §論点e, PR-1b's
// KindOpaque pattern): the key is still structurally recognized (so
// `boid config get/apply` never chokes on an old config.yaml that still sets
// it — see TestLoadFromPath_SandboxBackend_AcceptedButIgnored in
// config_test.go) but is no longer `boid config set`-able, the same
// read-only contract gateway.hosts already established.
func TestSet_KindOpaque_SandboxBackend_Rejected(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "sandbox.backend", []string{"container"}); err == nil {
		t.Fatal("expected Set(sandbox.backend, ...) to fail — it was removed in the volume-only cutover")
	}
}

// TestUnset_KindOpaque_SandboxBackend_Rejected mirrors
// TestUnset_KindOpaque_Rejected (gateway.hosts) for sandbox.backend.
func TestUnset_KindOpaque_SandboxBackend_Rejected(t *testing.T) {
	tree := Tree{"sandbox": Tree{"backend": "container"}}
	_, err := Unset(tree, "sandbox.backend")
	if err == nil {
		t.Fatal("expected Unset(sandbox.backend) to fail — it is read-only via the dotted-path CLI")
	}
	if strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected a read-only rejection, not a 'key not found' error: %v", err)
	}
}

func TestSet_DurationValidation(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "gc.interval", []string{"not-a-duration"}); err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if _, err := Set(tree, "gc.interval", []string{"48h"}); err != nil {
		t.Fatalf("Set valid duration: %v", err)
	}
}

func TestUnset_RemovesLeaf(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "web.public_url", []string{"https://x"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	reload, err := Unset(tree, "web.public_url")
	if err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
	if _, ok := GetPath(tree, "web.public_url"); ok {
		t.Error("key still present after unset")
	}
}

func TestUnset_NonExistentKeyFails(t *testing.T) {
	tree := Tree{}
	if _, err := Unset(tree, "web.public_url"); err == nil {
		t.Fatal("expected error: key not found")
	} else if !strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected 'key not found' error, got: %v", err)
	}
}

func TestUnset_UnknownKeyFails(t *testing.T) {
	tree := Tree{}
	if _, err := Unset(tree, "bogus.key"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestUnset_WholeForgeEntry(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "gateway.forges.github.host", []string{"github.com"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := Set(tree, "gateway.forges.github.secret_key", []string{"gh-pat"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	reload, err := Unset(tree, "gateway.forges.github")
	if err != nil {
		t.Fatalf("Unset whole entry: %v", err)
	}
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
	if _, ok := GetPath(tree, "gateway.forges.github"); ok {
		t.Error("whole forge entry still present after unset")
	}
	if _, ok := GetPath(tree, "gateway.forges.github.host"); ok {
		t.Error("forge entry field still present after whole-entry unset")
	}
}

func TestUnset_WholeForgeEntry_NotFound(t *testing.T) {
	tree := Tree{}
	if _, err := Unset(tree, "gateway.forges.nonexistent"); err == nil {
		t.Fatal("expected error: key not found")
	}
}

// TestUnset_WholeServiceEntry mirrors TestUnset_WholeForgeEntry for
// services.<name> (docs/plans/api-gateway.md §2, IsServiceEntryPath): a
// whole service entry removes the entire map entry, not just one leaf under
// it — the same "same as gateway.forges.github" parity
// schema_test.go's `{"services.myapp", false}` case comment claims for
// ResolveField, actually exercised here for Unset.
func TestUnset_WholeServiceEntry(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "services.myapp.base_url", []string{"https://myapp.example.com"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := Set(tree, "services.myapp.auth.kind", []string{"bearer"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := Set(tree, "services.myapp.auth.secret_key", []string{"myapp-token"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	reload, err := Unset(tree, "services.myapp")
	if err != nil {
		t.Fatalf("Unset whole entry: %v", err)
	}
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
	if _, ok := GetPath(tree, "services.myapp"); ok {
		t.Error("whole service entry still present after unset")
	}
	if _, ok := GetPath(tree, "services.myapp.base_url"); ok {
		t.Error("service entry field still present after whole-entry unset")
	}
	// The services map itself should also be pruned once its last entry is
	// gone — deletePathRaw's own "prune now-empty intermediate maps" rule
	// (dotted.go doc comment), the same behavior gateway.forges gets.
	if _, ok := GetPath(tree, "services"); ok {
		t.Error("services map itself should be pruned once empty")
	}
}

func TestUnset_WholeServiceEntry_NotFound(t *testing.T) {
	tree := Tree{}
	if _, err := Unset(tree, "services.nonexistent"); err == nil {
		t.Fatal("expected error: key not found")
	}
}

// TestGet_WholeServiceEntry mirrors Get's whole-forge-entry support
// (dotted.go's Get already special-cases IsForgeEntryPath so `boid config
// get gateway.forges.github` works despite that path not being a
// ResolveField leaf) for services.<name>.
func TestGet_WholeServiceEntry(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "services.myapp.base_url", []string{"https://myapp.example.com"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := Get(tree, "services.myapp")
	if err != nil {
		t.Fatalf("Get whole entry: %v", err)
	}
	m, ok := v.(Tree)
	if !ok {
		t.Fatalf("Get whole entry: got %T, want Tree", v)
	}
	if m["base_url"] != "https://myapp.example.com" {
		t.Errorf("base_url = %v, want https://myapp.example.com", m["base_url"])
	}
}

// TestUnset_WholeOAuthProviderEntry mirrors TestUnset_WholeServiceEntry for
// oauth_providers.<name> (docs/plans/api-gateway.md §6/§論点4, PR2,
// IsOAuthProviderEntryPath).
func TestUnset_WholeOAuthProviderEntry(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "oauth_providers.freee.token_endpoint", []string{"https://accounts.secure.freee.co.jp/public_api/token"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := Set(tree, "oauth_providers.freee.client_id", []string{"cid"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	reload, err := Unset(tree, "oauth_providers.freee")
	if err != nil {
		t.Fatalf("Unset whole entry: %v", err)
	}
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
	if _, ok := GetPath(tree, "oauth_providers.freee"); ok {
		t.Error("whole oauth_providers entry still present after unset")
	}
	if _, ok := GetPath(tree, "oauth_providers"); ok {
		t.Error("oauth_providers map itself should be pruned once empty")
	}
}

func TestUnset_WholeOAuthProviderEntry_NotFound(t *testing.T) {
	tree := Tree{}
	if _, err := Unset(tree, "oauth_providers.nonexistent"); err == nil {
		t.Fatal("expected error: key not found")
	}
}

// TestGet_WholeOAuthProviderEntry mirrors TestGet_WholeServiceEntry.
func TestGet_WholeOAuthProviderEntry(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "oauth_providers.freee.client_id", []string{"cid"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := Get(tree, "oauth_providers.freee")
	if err != nil {
		t.Fatalf("Get whole entry: %v", err)
	}
	m, ok := v.(Tree)
	if !ok {
		t.Fatalf("Get whole entry: got %T, want Tree", v)
	}
	if m["client_id"] != "cid" {
		t.Errorf("client_id = %v, want cid", m["client_id"])
	}
}

// TestSetGetUnset_OAuthProvidersFlowAndAuthorizeParams pins the PR3 (login
// flow) leaves' dotted-path round trip: oauth_providers.<name>.flow (a plain
// KindEnum leaf) and oauth_providers.<name>.authorize_params.<key> (the
// double-wildcard leaf schema.go added for the arbitrary provider-specific
// parameter map — docs/plans/api-gateway.md §7).
func TestSetGetUnset_OAuthProvidersFlowAndAuthorizeParams(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "oauth_providers.google.flow", []string{"loopback"}); err != nil {
		t.Fatalf("Set flow: %v", err)
	}
	if _, err := Set(tree, "oauth_providers.google.authorize_params.access_type", []string{"offline"}); err != nil {
		t.Fatalf("Set authorize_params.access_type: %v", err)
	}
	if v, err := Get(tree, "oauth_providers.google.flow"); err != nil || v != "loopback" {
		t.Errorf("Get flow = (%v, %v), want (loopback, nil)", v, err)
	}
	if v, err := Get(tree, "oauth_providers.google.authorize_params.access_type"); err != nil || v != "offline" {
		t.Errorf("Get authorize_params.access_type = (%v, %v), want (offline, nil)", v, err)
	}
	if _, err := Unset(tree, "oauth_providers.google.authorize_params.access_type"); err != nil {
		t.Fatalf("Unset authorize_params.access_type: %v", err)
	}
	if _, ok := GetPath(tree, "oauth_providers.google.authorize_params.access_type"); ok {
		t.Error("authorize_params.access_type still present after unset")
	}
	// flow itself must survive the sibling's removal.
	if v, err := Get(tree, "oauth_providers.google.flow"); err != nil || v != "loopback" {
		t.Errorf("Get flow after sibling unset = (%v, %v), want (loopback, nil)", v, err)
	}
}

func TestSet_OAuthProvidersFlow_UnrecognizedValueRejected(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "oauth_providers.google.flow", []string{"telepathy"}); err == nil {
		t.Fatal("want error for an unrecognized flow enum value, got nil")
	}
}

// TestUnset_KindOpaque_Rejected pins MINOR 1 (codex review round 2):
// gateway.hosts (the only KindOpaque leaf today) is documented as
// non-settable AND non-unsettable — Set already rejected it via
// coerceValues's dedicated KindOpaque branch (see
// TestValidateYAML_GatewayHosts_NotSettableViaDottedPath in validate_test.go),
// but the generic Unset path had no equivalent check and silently deleted
// it, letting `boid config unset gateway.hosts` "succeed" despite the
// documented read-only contract.
func TestUnset_KindOpaque_Rejected(t *testing.T) {
	tree := Tree{"gateway": Tree{"hosts": []any{
		Tree{"host": "github.com", "forge": "github", "secret_key": "gh-pat"},
	}}}
	_, err := Unset(tree, "gateway.hosts")
	if err == nil {
		t.Fatal("expected Unset(gateway.hosts) to fail — it is read-only via the dotted-path CLI")
	}
	if strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected a read-only rejection, not a 'key not found' error: %v", err)
	}
	// The key must survive the rejected unset attempt.
	if _, ok := GetPath(tree, "gateway.hosts"); !ok {
		t.Error("gateway.hosts was removed despite Unset returning an error")
	}
}

// TestUnset_KindOpaque_RejectedEvenWhenAbsent pins the same rejection when
// the key is not even present in tree — the read-only check must fire
// before the presence check, so the error is always "read-only", never
// "key not found", for a KindOpaque leaf.
func TestUnset_KindOpaque_RejectedEvenWhenAbsent(t *testing.T) {
	tree := Tree{}
	_, err := Unset(tree, "gateway.hosts")
	if err == nil {
		t.Fatal("expected Unset(gateway.hosts) to fail even when absent")
	}
	if strings.Contains(err.Error(), "key not found") {
		t.Errorf("expected a read-only rejection, not a 'key not found' error: %v", err)
	}
}

func TestGet_UnknownKey(t *testing.T) {
	tree := Tree{}
	_, err := Get(tree, "sandbox.alowed_domains")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestGet_KnownButAbsent(t *testing.T) {
	tree := Tree{}
	_, err := Get(tree, "web.public_url")
	if err == nil {
		t.Fatal("expected error: key not found")
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSet_KindInt covers the KindInt coercion path added for the egress
// proxy port band — the only KindInt leaves in the schema today.
//
// Worth pinning explicitly because these two keys are unusual: the band's
// paired-values rule means a single `boid config set
// sandbox.egress_proxy_port_low ...` against a config that has neither key
// set is rejected by validation downstream, so this coercion is mostly
// reached via `boid config edit`/`apply`. That makes it exactly the kind of
// code path that rots unnoticed.
func TestSet_KindInt(t *testing.T) {
	tree := Tree{}
	reload, err := Set(tree, "sandbox.egress_proxy_port_low", []string{"20000"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if reload != ReloadRestartRequired {
		t.Errorf("reload = %v, want ReloadRestartRequired", reload)
	}
	got, ok := GetPath(tree, "sandbox.egress_proxy_port_low")
	if !ok {
		t.Fatal("GetPath: key absent after Set")
	}
	// Stored as an int, not the raw string: a string would round-trip into
	// config.yaml quoted and then fail the int decode on the next load.
	if n, isInt := got.(int); !isInt || n != 20000 {
		t.Errorf("GetPath = %#v, want int 20000", got)
	}
}

func TestSet_KindInt_RejectsNonInteger(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "sandbox.egress_proxy_port_low", []string{"not-a-number"}); err == nil {
		t.Error("Set with a non-integer value = nil, want an error")
	}
}

func TestSet_KindInt_RejectsMultipleValues(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "sandbox.egress_proxy_port_low", []string{"20000", "20999"}); err == nil {
		t.Error("Set with two values for a scalar int = nil, want an error")
	}
}

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
	if reload != ReloadDynamic {
		t.Errorf("reload = %v, want ReloadDynamic", reload)
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

func TestSet_EnumValidation(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "sandbox.backend", []string{"bogus"}); err == nil {
		t.Fatal("expected error for invalid enum value")
	}
	reload, err := Set(tree, "sandbox.backend", []string{"container"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if reload != ReloadRetirementWarning {
		t.Errorf("reload = %v, want ReloadRetirementWarning", reload)
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
	if reload != ReloadDynamic {
		t.Errorf("reload = %v, want ReloadDynamic", reload)
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

package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestServiceConfigSchema_Exhaustive is the mechanical guard item 1 and
// item 4 of the PR #1040 opus review (docs/plans/api-gateway-credential-
// accounts.md) asked for: config.ServiceConfig picked up two new bool
// fields (AllowReadOnlyWrite, then RequireAccount) whose dotted paths
// (services.<name>.allow_readonly_write / .require_account) were never
// added to internal/config/schema.go's Schema — so ValidateKnownKeys
// rejected any config.yaml document that set them as an "unknown config
// key" (see TestValidateYAML_Services_RequireAccountAndAllowReadOnlyWrite_
// Accepted, right above). git log -S confirms allow_readonly_write shipped
// with this exact gap; require_account then repeated it by copying the
// adjacent field's pattern rather than schema.go's own registration.
//
// This test walks config.ServiceConfig's fields via reflection — using
// each field's yaml tag as its dotted-path leaf name under "services.*",
// recursing one level into ServiceAuthConfig for "services.*.auth.*", and
// treating Credentials (a map[string]string) as the existing second-
// wildcard-segment shape "services.*.credentials.*" — and fails, naming
// the missing dotted path, for any field Schema does not recognize. A
// future field added to ServiceConfig without a matching schema.go entry
// fails THIS test at `go test` time instead of only surfacing the first
// time an operator's config.yaml happens to contain it.
func TestServiceConfigSchema_Exhaustive(t *testing.T) {
	assertServiceFieldsInSchema(t, "services.myapp", reflect.TypeOf(ServiceConfig{}))
}

// assertServiceFieldsInSchema recurses into nested struct fields (today:
// just ServiceConfig.Auth) one level at a time — deep enough for the
// current two-level services.<name>.auth.<leaf> shape without needing a
// generic arbitrary-depth walker no schema.go entry actually uses yet.
func assertServiceFieldsInSchema(t *testing.T, prefix string, typ reflect.Type) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := yamlLeafName(f)
		if name == "" {
			continue
		}
		path := prefix + "." + name
		switch f.Type.Kind() {
		case reflect.Struct:
			assertServiceFieldsInSchema(t, path, f.Type)
		case reflect.Map:
			// A second wildcard segment (e.g. services.*.credentials.*) —
			// resolve a concrete synthetic key underneath it, same
			// convention oauth_providers.*.authorize_params.* already
			// uses in schema.go.
			probe := path + ".somekey"
			if _, ok := ResolveField(probe); !ok {
				t.Errorf("%s.%s: no schema.go entry matches dotted path %q (add a %q entry to Schema)", typ.Name(), f.Name, probe, path+".*")
			}
		default:
			if _, ok := ResolveField(path); !ok {
				t.Errorf("%s.%s: no schema.go entry matches dotted path %q (add it to Schema)", typ.Name(), f.Name, path)
			}
		}
	}
}

// yamlLeafName extracts a struct field's yaml tag name (before any comma
// option like ",omitempty"), or "" if the field has no yaml tag / is
// explicitly "-".
func yamlLeafName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" || tag == "-" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

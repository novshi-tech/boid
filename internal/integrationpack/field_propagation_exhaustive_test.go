package integrationpack

import (
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/config"
)

// serviceConfigPassthroughExclusions names every config.ServiceConfig field
// that shares BOTH its name and its Go type with a field on
// apigateway.ServiceConfig, but is NOT a "copy verbatim through both
// conversion paths" field — TestServiceConfigFieldPropagation_Exhaustive
// (below) matches purely on name+type, so anything excluded here needs a
// documented reason for why that naive match would be wrong.
var serviceConfigPassthroughExclusions = map[string]string{
	"BaseURL": "config.Config.APIGatewayServices() copies sc.BaseURL " +
		"directly, but DesugarService derives apigateway.ServiceConfig." +
		"BaseURL from sc.Endpoint instead (a uses: entry has no base_url " +
		"of its own) — the two paths deliberately read DIFFERENT source " +
		"fields into this one field, so a naive same-name/same-type " +
		"passthrough check would be wrong here.",
}

// TestServiceConfigFieldPropagation_Exhaustive is the item-4 exhaustiveness
// guard the PR #1040 opus review (docs/plans/api-gateway-credential-
// accounts.md) asked for. config.ServiceConfig -> apigateway.ServiceConfig
// has TWO independent conversion paths — config.Config.APIGatewayServices()
// for free-form base_url/auth entries, and DesugarService (resolve.go) for
// uses: entries — and until now the only regression coverage was two
// hand-written, per-field tests (TestDesugarService_
// PropagatesAllowReadOnlyWrite / _PropagatesRequireAccount, plus config's
// own field-mapping tests) that exist only because those two specific
// fields already caused review findings once each. Neither fires again for
// a THIRD field that ships wired into only one of the two paths.
//
// This test finds every config.ServiceConfig field that shares both its
// name and its Go type with an apigateway.ServiceConfig field — today:
// AllowReadOnlyWrite and RequireAccount (BaseURL also matches by name/type
// but is excluded above, since the two paths deliberately read it from
// different sources) — and, for each one, sets a distinguishing value and
// round-trips it through BOTH APIGatewayServices() and DesugarService(),
// failing loudly (naming the path that dropped it) if either does not
// carry it through. Run TestServiceConfigFieldPropagation_Exhaustive with
// one of the two RequireAccount/AllowReadOnlyWrite assignments in
// resolve.go's DesugarService commented out to see this fail — it does
// (verified manually while writing this test).
//
// A future field added to BOTH structs under the same name and type is
// picked up automatically by the loop below; the `want` set at the end
// forces a conscious update to this test file (not a silent pass) the
// first time that happens, so an omission from either conversion path is
// caught here rather than the next time an operator's config.yaml happens
// to exercise the gap.
func TestServiceConfigFieldPropagation_Exhaustive(t *testing.T) {
	scType := reflect.TypeOf(config.ServiceConfig{})
	gwType := reflect.TypeOf(apigateway.ServiceConfig{})

	gwFieldsByName := make(map[string]reflect.StructField, gwType.NumField())
	for i := 0; i < gwType.NumField(); i++ {
		f := gwType.Field(i)
		gwFieldsByName[f.Name] = f
	}

	var checked []string
	for i := 0; i < scType.NumField(); i++ {
		scField := scType.Field(i)
		gwField, ok := gwFieldsByName[scField.Name]
		if !ok || gwField.Type != scField.Type {
			continue
		}
		if reason, excluded := serviceConfigPassthroughExclusions[scField.Name]; excluded {
			if reason == "" {
				t.Errorf("serviceConfigPassthroughExclusions[%q] has an empty reason", scField.Name)
			}
			continue
		}
		if scField.Type.Kind() != reflect.Bool {
			t.Errorf("field %q matches config.ServiceConfig/apigateway.ServiceConfig by name+type but this test only knows how to synthesize a distinguishing value for bool fields — extend it (or add an exclusion with a reason) before shipping this field", scField.Name)
			continue
		}

		fieldName := scField.Name
		checked = append(checked, fieldName)
		t.Run(fieldName, func(t *testing.T) {
			// Free-form (base_url/auth) path: config.Config.APIGatewayServices().
			flat := config.ServiceConfig{
				BaseURL: "https://example.com",
				Auth:    config.ServiceAuthConfig{Kind: "bearer", SecretKey: "tok"},
			}
			reflect.ValueOf(&flat).Elem().FieldByName(fieldName).SetBool(true)
			flatCfg := &config.Config{Services: map[string]config.ServiceConfig{"svc": flat}}
			flatOut := flatCfg.APIGatewayServices()
			if len(flatOut) != 1 {
				t.Fatalf("APIGatewayServices() = %v, want exactly 1 entry", flatOut)
			}
			if got := reflect.ValueOf(flatOut[0]).FieldByName(fieldName).Bool(); !got {
				t.Errorf("APIGatewayServices(): %s = false, want true (dropped by the free-form base_url/auth path)", fieldName)
			}

			// uses: path: DesugarService().
			uses := config.ServiceConfig{
				Uses:        "jira-cloud/jira-cloud@1.2.0",
				Endpoint:    "https://example.atlassian.net",
				Credentials: map[string]string{"token": "JIRA_TOKEN"},
			}
			reflect.ValueOf(&uses).Elem().FieldByName(fieldName).SetBool(true)
			usesOut, err := DesugarService("svc", uses, bearerPack(t))
			if err != nil {
				t.Fatalf("DesugarService: %v", err)
			}
			if got := reflect.ValueOf(usesOut).FieldByName(fieldName).Bool(); !got {
				t.Errorf("DesugarService(): %s = false, want true (dropped by the uses: path)", fieldName)
			}
		})
	}

	want := map[string]bool{"AllowReadOnlyWrite": true, "RequireAccount": true}
	if len(checked) != len(want) {
		t.Fatalf("round-trip-checked fields = %v, want exactly %v — a new same-name/same-type field appeared on both structs: give it a round-trip check (it will run automatically) and update `want` here, or add it to serviceConfigPassthroughExclusions with a reason", checked, want)
	}
	for _, name := range checked {
		if !want[name] {
			t.Errorf("unexpected field %q was round-trip checked — update `want` in this test", name)
		}
	}
}

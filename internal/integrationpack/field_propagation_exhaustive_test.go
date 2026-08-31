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

// serviceConfigFreeFormOnlyFields names every config.ServiceConfig field
// that IS a genuine "copy verbatim" passthrough field, but ONLY on the
// free-form (APIGatewayServices()) conversion path — unlike BaseURL above
// (which the uses: path propagates from a DIFFERENT source field),
// DesugarService has no equivalent concept for these fields AT ALL, so its
// output can never carry a value to round-trip-check against.
// TestServiceConfigFieldPropagation_Exhaustive still checks these through
// APIGatewayServices() (the actual code path an operator's free-form
// config.yaml entry exercises) rather than skipping them outright the way
// serviceConfigPassthroughExclusions does — this is what closed the F1 gap
// (Opus review, PR #1042): BaseURLSecretKey used to sit in the exclusion
// map above with no coverage anywhere, and mutation testing showed deleting
// its one propagation line in APIGatewayServices() left the whole `go test
// ./...` suite green.
var serviceConfigFreeFormOnlyFields = map[string]string{
	"BaseURLSecretKey": "a uses: entry's base_url always comes from " +
		"sc.Endpoint via DesugarService (the same reason BaseURL itself " +
		"is excluded above) — base_url_secret_key has no equivalent on " +
		"the uses: path at all (validateServiceConfig rejects setting it " +
		"alongside uses: outright, docs/plans/api-gateway-credential-" +
		"accounts.md D12), so DesugarService's output never carries a " +
		"value for it to round-trip-check against.",
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
// AllowReadOnlyWrite, RequireAccount, AllowInsecure (round-tripped through
// both paths), and BaseURLSecretKey (round-tripped through
// APIGatewayServices() only — see serviceConfigFreeFormOnlyFields above;
// BaseURL itself is fully excluded, see serviceConfigPassthroughExclusions)
// — and, for each one, sets a distinguishing value (bool or string; see
// setDistinguishingValue) and checks it survives the applicable
// conversion(s), failing loudly (naming the path that dropped it) if not.
// Run TestServiceConfigFieldPropagation_Exhaustive with one of the two
// RequireAccount/AllowReadOnlyWrite assignments in resolve.go's
// DesugarService commented out to see this fail — it does (verified
// manually while writing this test).
//
// This test's own doc comment used to claim to be exhaustive while only
// ever looking at TOP-LEVEL ServiceConfig fields — auth.username_secret_key
// (D12) lives on the nested Auth block and was invisible to this walker
// (config.ServiceConfig.Auth is type ServiceAuthConfig,
// apigateway.ServiceConfig.Auth is type ServiceAuth — different types, so
// the gwField.Type != scField.Type check below always skips over it without
// ever looking inside). See TestServiceConfigFieldPropagation_Exhaustive_Auth
// for that nested counterpart — a genuinely separate test, not a recursive
// extension of this one, because the two paths' contracts differ
// structurally for Auth fields (see that test's own doc comment for why).
//
// A future field added to BOTH top-level structs under the same name and
// type is picked up automatically by the loop below; the `want` set at the
// end forces a conscious update to this test file (not a silent pass) the
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
		kind := scField.Type.Kind()
		if kind != reflect.Bool && kind != reflect.String {
			t.Errorf("field %q matches config.ServiceConfig/apigateway.ServiceConfig by name+type but this test only knows how to synthesize a distinguishing value for bool/string fields — extend it (or add an exclusion with a reason) before shipping this field", scField.Name)
			continue
		}

		fieldName := scField.Name
		freeFormOnlyReason, freeFormOnly := serviceConfigFreeFormOnlyFields[fieldName]
		checked = append(checked, fieldName)
		t.Run(fieldName, func(t *testing.T) {
			// Free-form (base_url/auth) path: config.Config.APIGatewayServices().
			flat := config.ServiceConfig{
				BaseURL: "https://example.com",
				Auth:    config.ServiceAuthConfig{Kind: "bearer", SecretKey: "tok"},
			}
			if fieldName == "BaseURLSecretKey" {
				// BaseURLSecretKey and the fixture's own literal BaseURL
				// above are mutually exclusive (validateServiceConfig) —
				// clear the baseline so setting BaseURLSecretKey below
				// doesn't fail config validation and silently produce zero
				// output entries instead of exercising the propagation
				// this test actually wants to check.
				flat.BaseURL = ""
			}
			setDistinguishingValue(t, reflect.ValueOf(&flat).Elem().FieldByName(fieldName), kind)
			flatCfg := &config.Config{Services: map[string]config.ServiceConfig{"svc": flat}}
			flatOut := flatCfg.APIGatewayServices()
			if len(flatOut) != 1 {
				t.Fatalf("APIGatewayServices() = %v, want exactly 1 entry", flatOut)
			}
			checkDistinguishingValue(t, reflect.ValueOf(flatOut[0]).FieldByName(fieldName), kind, "APIGatewayServices(): "+fieldName)

			if freeFormOnly {
				t.Logf("%s: skipping uses:/DesugarService round-trip — %s", fieldName, freeFormOnlyReason)
				return
			}

			// uses: path: DesugarService().
			uses := config.ServiceConfig{
				Uses:        "jira-cloud/jira-cloud@1.2.0",
				Endpoint:    "https://example.atlassian.net",
				Credentials: map[string]string{"token": "JIRA_TOKEN"},
			}
			setDistinguishingValue(t, reflect.ValueOf(&uses).Elem().FieldByName(fieldName), kind)
			usesOut, err := DesugarService("svc", uses, bearerPack(t))
			if err != nil {
				t.Fatalf("DesugarService: %v", err)
			}
			checkDistinguishingValue(t, reflect.ValueOf(usesOut).FieldByName(fieldName), kind, "DesugarService(): "+fieldName)
		})
	}

	// AllowInsecure (docs/plans/api-gateway-credential-accounts.md D12) is a
	// new same-name/same-type field on BOTH structs as of this change — see
	// the DesugarService/resolve.go propagation this test's own failure
	// pointed at before that fix landed. BaseURLSecretKey is checked here
	// too now (free-form path only — serviceConfigFreeFormOnlyFields).
	want := map[string]bool{"AllowReadOnlyWrite": true, "RequireAccount": true, "AllowInsecure": true, "BaseURLSecretKey": true}
	if len(checked) != len(want) {
		t.Fatalf("round-trip-checked fields = %v, want exactly %v — a new same-name/same-type field appeared on both structs: give it a round-trip check (it will run automatically) and update `want` here, or add it to serviceConfigPassthroughExclusions/serviceConfigFreeFormOnlyFields with a reason", checked, want)
	}
	for _, name := range checked {
		if !want[name] {
			t.Errorf("unexpected field %q was round-trip checked — update `want` in this test", name)
		}
	}
}

// TestServiceConfigFieldPropagation_Exhaustive_Auth is
// TestServiceConfigFieldPropagation_Exhaustive's counterpart for
// config.ServiceAuthConfig -> apigateway.ServiceAuth (the nested "auth:"
// block) — see that test's own doc comment for why the top-level walker
// never looks inside Auth at all (a type mismatch on the Auth field itself
// short-circuits it before ever reaching this level).
//
// Unlike the top-level walker, this one checks ONLY the free-form
// (APIGatewayServices()) path, for every matching field, unconditionally —
// not per-field via an exclusion/opt-out map. A uses: entry's "auth:" block
// must be the exact zero value config.ServiceAuthConfig{}
// (validateServiceConfig rejects setting uses: and auth: together), so
// DesugarService never reads sc.Auth AT ALL for a uses: entry — its
// apigateway.ServiceAuth is built entirely from the resolved Integration
// Pack profile's credential slot instead (see DesugarService's own doc
// comment). There is therefore no meaningful "did auth.<field> survive the
// uses: path" question to ask for ANY ServiceAuthConfig field — a
// structural property of the two conversion paths, not a per-field
// exception the way BaseURL/BaseURLSecretKey need on the top-level struct.
//
// This is the test that actually closes the gap Opus review found in PR
// #1042 (F1): auth.username_secret_key (D12) shipped with zero coverage
// proving config.Config.APIGatewayServices() propagates
// config.ServiceAuthConfig.UsernameSecretKey into
// apigateway.ServiceAuth.UsernameSecretKey. Mutation-verified: deleting
// that one line from APIGatewayServices() left `go test ./...` green
// end-to-end before this test existed.
func TestServiceConfigFieldPropagation_Exhaustive_Auth(t *testing.T) {
	scAuthType := reflect.TypeOf(config.ServiceAuthConfig{})
	gwAuthType := reflect.TypeOf(apigateway.ServiceAuth{})

	gwAuthFieldsByName := make(map[string]reflect.StructField, gwAuthType.NumField())
	for i := 0; i < gwAuthType.NumField(); i++ {
		f := gwAuthType.Field(i)
		gwAuthFieldsByName[f.Name] = f
	}

	var checked []string
	for i := 0; i < scAuthType.NumField(); i++ {
		scField := scAuthType.Field(i)
		gwField, ok := gwAuthFieldsByName[scField.Name]
		if !ok || gwField.Type != scField.Type {
			// Kind is deliberately excluded by this check, not a
			// maintained exclusion list: config.ServiceAuthConfig.Kind is
			// a plain string, apigateway.ServiceAuth.Kind is the defined
			// type AuthKind — they never match by exact reflect.Type
			// equality, so Kind never reaches the loop body below at all.
			continue
		}
		if scField.Type.Kind() != reflect.String {
			t.Errorf("field %q matches config.ServiceAuthConfig/apigateway.ServiceAuth by name+type but this test only knows how to synthesize a distinguishing value for string fields — extend it before shipping this field", scField.Name)
			continue
		}

		fieldName := scField.Name
		checked = append(checked, fieldName)
		t.Run("Auth."+fieldName, func(t *testing.T) {
			flat := config.ServiceConfig{
				BaseURL: "https://example.com",
				Auth:    config.ServiceAuthConfig{Kind: "bearer", SecretKey: "tok"},
			}
			reflect.ValueOf(&flat.Auth).Elem().FieldByName(fieldName).SetString("distinguishing-value")
			flatCfg := &config.Config{Services: map[string]config.ServiceConfig{"svc": flat}}
			flatOut := flatCfg.APIGatewayServices()
			if len(flatOut) != 1 {
				t.Fatalf("APIGatewayServices() = %v, want exactly 1 entry", flatOut)
			}
			if got := reflect.ValueOf(flatOut[0].Auth).FieldByName(fieldName).String(); got != "distinguishing-value" {
				t.Errorf("APIGatewayServices(): Auth.%s = %q, want %q (dropped by the free-form auth path)", fieldName, got, "distinguishing-value")
			}
		})
	}

	want := map[string]bool{"SecretKey": true, "Username": true, "Header": true, "Query": true, "Provider": true, "UsernameSecretKey": true}
	if len(checked) != len(want) {
		t.Fatalf("round-trip-checked auth fields = %v, want exactly %v — a new same-name/same-type field appeared on both structs: give it a round-trip check (it will run automatically) and update `want` here", checked, want)
	}
	for _, name := range checked {
		if !want[name] {
			t.Errorf("unexpected auth field %q was round-trip checked — update `want` in this test", name)
		}
	}
}

// setDistinguishingValue writes a value into v (a settable field of the
// given kind) that checkDistinguishingValue can later tell apart from that
// field's zero value — bool's only non-zero value is true; string uses a
// fixed sentinel so a copy-paste mistake that sets the WRONG field still
// shows up as "want %q, got %q" rather than two fields silently agreeing.
func setDistinguishingValue(t *testing.T, v reflect.Value, kind reflect.Kind) {
	t.Helper()
	switch kind {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("distinguishing-value")
	default:
		t.Fatalf("setDistinguishingValue: unsupported kind %v", kind)
	}
}

// checkDistinguishingValue is setDistinguishingValue's assertion
// counterpart — label identifies which conversion function/field the value
// came from, for the failure message.
func checkDistinguishingValue(t *testing.T, v reflect.Value, kind reflect.Kind, label string) {
	t.Helper()
	switch kind {
	case reflect.Bool:
		if !v.Bool() {
			t.Errorf("%s = false, want true (dropped)", label)
		}
	case reflect.String:
		if got := v.String(); got != "distinguishing-value" {
			t.Errorf("%s = %q, want %q (dropped)", label, got, "distinguishing-value")
		}
	default:
		t.Fatalf("checkDistinguishingValue: unsupported kind %v", kind)
	}
}

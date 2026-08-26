package server

// docs/plans/signal-driven-review.md §14 Q18: "daemon が SaaS credential を
// 保持して外部 API を直接呼ぶ経路が存在しない (connector の外部到達は
// workspace sandbox + API gateway 経由のみ)". This is a structural claim
// about connector_exec.go specifically — the file that resolves a
// connector's env/bind/service-allowlist for StartExec — mirroring
// internal/integrationpack/import_guard_test.go's TestQ16_NoDBOrDispatcherImport
// style: parse the file's own import block and assert it never imports
// internal/apigateway (the ONLY package in this repo that resolves a
// service's credential — SecretResolver/CredentialProvider,
// internal/apigateway/credentials.go) or internal/dispatcher (the sandbox/
// broker-registration layer). connector_exec.go has no business EVER
// holding a credential value: it builds env vars that name a SERVICE
// (BOID_SIGNAL_SERVICE) for the connector to reach through
// $BOID_API_BASE/<service>/..., never a bearer token/secret itself — the
// API gateway server (internal/apigateway, wired independently in wire.go,
// untouched by this PR) is the only place credential injection happens.

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestQ18_ConnectorExecNeverImportsAPIGatewayCredentials(t *testing.T) {
	forbidden := []string{
		"github.com/novshi-tech/boid/internal/apigateway",
		"github.com/novshi-tech/boid/internal/dispatcher",
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "connector_exec.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse connector_exec.go: %v", err)
	}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s: %v", imp.Path.Value, err)
		}
		for _, bad := range forbidden {
			if path == bad {
				t.Errorf("connector_exec.go: forbidden import %q (Q18: connector resolution must never touch credential/dispatch machinery directly)", path)
			}
		}
	}
}

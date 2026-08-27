package server

// docs/plans/signal-ingest-detailed-design.md §5.2 (PR-5): resolveConnectorExec
// resolves a StartExecRequest's raw ConnectorRef against the daemon's
// loaded Pack registry, producing the env/bind/service-allowlist a
// connector job's SessionJobInput needs. Pure function, no server/DB
// wiring, tested in isolation here; sessionDispatcherAdapter.StartExec
// (wire.go) is the only caller.

import (
	"testing"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/integrationpack"
)

func samplePack() *integrationpack.Pack {
	return &integrationpack.Pack{
		Name:    "slack",
		Version: "1.1.0",
		Dir:     "/opt/boid/integrations/slack/1.1.0",
		Manifest: integrationpack.Manifest{
			Connectors: []integrationpack.Connector{
				{Name: "mentions", Executable: "connectors/mentions", ServiceProfile: "slack-cloud"},
			},
		},
	}
}

func sampleRef() apiwire.ConnectorRef {
	return apiwire.ConnectorRef{
		Pack:          "slack",
		ConnectorName: "mentions",
		Service:       "slack-api",
		Config:        map[string]any{"include_threads": true},
	}
}

func TestResolveConnectorExec_HappyPath(t *testing.T) {
	got, err := resolveConnectorExec([]*integrationpack.Pack{samplePack()}, sampleRef())
	if err != nil {
		t.Fatalf("resolveConnectorExec: %v", err)
	}
	if got.Env["BOID_SIGNAL_SERVICE"] != "slack-api" {
		t.Errorf("BOID_SIGNAL_SERVICE = %q, want slack-api", got.Env["BOID_SIGNAL_SERVICE"])
	}
	if got.Env["BOID_SIGNAL_CONNECTOR"] != "slack/mentions" {
		t.Errorf("BOID_SIGNAL_CONNECTOR = %q, want slack/mentions", got.Env["BOID_SIGNAL_CONNECTOR"])
	}
	// daemon と job container は同じ base image を共有する (compose.yml
	// 決定2「daemon と job runner が同じ versioned base イメージから起動」) —
	// pack.Dir (daemon プロセスが読み取りに使う integrations.dir 配下の絶対
	// パス) は job container 自身のファイルシステムにも同じ場所に既に
	// 存在するので、bind mount で運ぶ必要が無い。BOID_CONNECTOR_EXEC は
	// pack.Dir をそのまま指す。
	if got.Env["BOID_CONNECTOR_EXEC"] != "/opt/boid/integrations/slack/1.1.0/connectors/mentions" {
		t.Errorf("BOID_CONNECTOR_EXEC = %q, want /opt/boid/integrations/slack/1.1.0/connectors/mentions (pack.Dir, no bind mount)", got.Env["BOID_CONNECTOR_EXEC"])
	}
	if got.Env["BOID_SIGNAL_CONFIG"] != `{"include_threads":true}` {
		t.Errorf("BOID_SIGNAL_CONFIG = %q, want {\"include_threads\":true}", got.Env["BOID_SIGNAL_CONFIG"])
	}
	if len(got.APIGatewayServices) != 1 || got.APIGatewayServices[0] != "slack-api" {
		t.Errorf("APIGatewayServices = %v, want [slack-api]", got.APIGatewayServices)
	}
}

func TestResolveConnectorExec_NoConfig_EmptyJSONObject(t *testing.T) {
	ref := sampleRef()
	ref.Config = nil
	got, err := resolveConnectorExec([]*integrationpack.Pack{samplePack()}, ref)
	if err != nil {
		t.Fatalf("resolveConnectorExec: %v", err)
	}
	if got.Env["BOID_SIGNAL_CONFIG"] != "{}" {
		t.Errorf("BOID_SIGNAL_CONFIG = %q, want {}", got.Env["BOID_SIGNAL_CONFIG"])
	}
}

func TestResolveConnectorExec_PackNotInstalled(t *testing.T) {
	ref := sampleRef()
	ref.Pack = "unknown-pack"
	if _, err := resolveConnectorExec([]*integrationpack.Pack{samplePack()}, ref); err == nil {
		t.Fatal("resolveConnectorExec() = nil error, want a rejection for an uninstalled pack")
	}
}

func TestResolveConnectorExec_AmbiguousPackVersion_Rejected(t *testing.T) {
	packs := []*integrationpack.Pack{
		samplePack(),
		{Name: "slack", Version: "2.0.0", Dir: "/opt/boid/integrations/slack/2.0.0", Manifest: integrationpack.Manifest{
			Connectors: []integrationpack.Connector{{Name: "mentions", Executable: "connectors/mentions"}},
		}},
	}
	if _, err := resolveConnectorExec(packs, sampleRef()); err == nil {
		t.Fatal("resolveConnectorExec() = nil error, want a rejection for two installed versions of the same pack (v0 has no version pin in signals.sources)")
	}
}

func TestResolveConnectorExec_ConnectorNotFoundInPack(t *testing.T) {
	ref := sampleRef()
	ref.ConnectorName = "no-such-connector"
	if _, err := resolveConnectorExec([]*integrationpack.Pack{samplePack()}, ref); err == nil {
		t.Fatal("resolveConnectorExec() = nil error, want a rejection for an unknown connector name")
	}
}

// TestResolveConnectorExec_ConfigSchemaValidation_Rejected pins §6.2 item 4:
// a connector's declared configSchema validates signals.sources[].config —
// a config missing a required field is rejected.
func TestResolveConnectorExec_ConfigSchemaValidation_Rejected(t *testing.T) {
	pack := samplePack()
	pack.Manifest.Connectors[0].ConfigSchema = &integrationpack.ConfigSchema{
		Type:     "object",
		Required: []string{"jql"},
		Properties: map[string]integrationpack.PropertySchema{
			"jql": {Type: "string"},
		},
	}
	ref := sampleRef() // Config has no "jql" key
	if _, err := resolveConnectorExec([]*integrationpack.Pack{pack}, ref); err == nil {
		t.Fatal("resolveConnectorExec() = nil error, want a rejection for a config missing a required schema field")
	}
}

func TestResolveConnectorExec_ConfigSchemaValidation_Passes(t *testing.T) {
	pack := samplePack()
	pack.Manifest.Connectors[0].ConfigSchema = &integrationpack.ConfigSchema{
		Type: "object",
		Properties: map[string]integrationpack.PropertySchema{
			"include_threads": {Type: "boolean"},
		},
	}
	got, err := resolveConnectorExec([]*integrationpack.Pack{pack}, sampleRef())
	if err != nil {
		t.Fatalf("resolveConnectorExec: %v", err)
	}
	if got.Env["BOID_SIGNAL_CONFIG"] == "" {
		t.Error("expected BOID_SIGNAL_CONFIG to be set")
	}
}

package orchestrator

// docs/plans/signal-ingest-detailed-design.md §5.1 (PR-5): deriveSignalTriggers
// turns a project.yaml `signals.sources[]` list into the derived Trigger
// entries hydrate appends to meta.Triggers (source 1 件 = trigger 1 本). This
// file pins the pure derivation function in isolation; the full
// parseProjectMetaBytes pipeline (append + ValidateTriggers collision
// detection) is pinned by trigger_spec_loader_test.go's
// TestReadProjectMeta_Signals_* tests (package orchestrator_test, since those
// drive the public ReadProjectMeta entry point).

import (
	"testing"
)

func TestDeriveSignalTriggers_Empty(t *testing.T) {
	got, err := deriveSignalTriggers(nil)
	if err != nil {
		t.Fatalf("deriveSignalTriggers(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deriveSignalTriggers(nil) = %+v, want empty", got)
	}
}

func TestDeriveSignalTriggers_OneSource_NameEveryRun(t *testing.T) {
	sources := []SignalSource{
		{Connector: "slack/mentions", Service: "slack-api", Every: "10m", Config: map[string]any{"include_threads": true}},
	}
	got, err := deriveSignalTriggers(sources)
	if err != nil {
		t.Fatalf("deriveSignalTriggers error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("deriveSignalTriggers = %+v, want 1 entry", got)
	}
	trig := got[0]
	if trig.Name != "signal:slack/mentions" {
		t.Errorf("Name = %q, want %q", trig.Name, "signal:slack/mentions")
	}
	if trig.Every != "10m" {
		t.Errorf("Every = %q, want %q", trig.Every, "10m")
	}
	if trig.Run != `exec "$BOID_CONNECTOR_EXEC"` {
		t.Errorf("Run = %q, want the fixed BOID_CONNECTOR_EXEC invocation", trig.Run)
	}
	if trig.On != "" {
		t.Errorf("On = %q, want empty (schedule default — a connector trigger is NOT on:signals)", trig.On)
	}
	if trig.Connector == nil {
		t.Fatal("Connector metadata is nil, want populated TriggerConnector")
	}
	if trig.Connector.Pack != "slack" || trig.Connector.ConnectorName != "mentions" {
		t.Errorf("Connector = %+v, want Pack=slack ConnectorName=mentions", trig.Connector)
	}
	if trig.Connector.Service != "slack-api" {
		t.Errorf("Connector.Service = %q, want %q", trig.Connector.Service, "slack-api")
	}
	if trig.Connector.Config["include_threads"] != true {
		t.Errorf("Connector.Config = %+v, want include_threads=true", trig.Connector.Config)
	}
}

func TestDeriveSignalTriggers_MultipleSources_DistinctTriggers(t *testing.T) {
	sources := []SignalSource{
		{Connector: "slack/mentions", Service: "slack-api", Every: "10m"},
		{Connector: "jira-cloud/assigned-issues", Service: "customer-jira", Every: "5m"},
	}
	got, err := deriveSignalTriggers(sources)
	if err != nil {
		t.Fatalf("deriveSignalTriggers error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("deriveSignalTriggers = %+v, want 2 entries", got)
	}
	if got[0].Name != "signal:slack/mentions" || got[1].Name != "signal:jira-cloud/assigned-issues" {
		t.Errorf("names = [%q, %q], unexpected", got[0].Name, got[1].Name)
	}
}

func TestDeriveSignalTriggers_DuplicateSource_Rejected(t *testing.T) {
	sources := []SignalSource{
		{Connector: "slack/mentions", Service: "slack-api", Every: "10m"},
		{Connector: "slack/mentions", Service: "slack-api-2", Every: "20m"},
	}
	if _, err := deriveSignalTriggers(sources); err == nil {
		t.Fatal("deriveSignalTriggers() = nil error, want a rejection for the duplicate source")
	}
}

func TestDeriveSignalTriggers_MalformedConnectorRef_Rejected(t *testing.T) {
	cases := []string{"slack", "slack/", "/mentions", "slack/mentions/extra"}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			sources := []SignalSource{{Connector: ref, Service: "svc", Every: "10m"}}
			if _, err := deriveSignalTriggers(sources); err == nil {
				t.Fatalf("deriveSignalTriggers(%q) = nil error, want a rejection", ref)
			}
		})
	}
}

func TestDeriveSignalTriggers_MissingService_Rejected(t *testing.T) {
	sources := []SignalSource{{Connector: "slack/mentions", Every: "10m"}}
	if _, err := deriveSignalTriggers(sources); err == nil {
		t.Fatal("deriveSignalTriggers() = nil error, want a rejection for missing service")
	}
}

func TestDeriveSignalTriggers_MissingEvery_Rejected(t *testing.T) {
	sources := []SignalSource{{Connector: "slack/mentions", Service: "slack-api"}}
	if _, err := deriveSignalTriggers(sources); err == nil {
		t.Fatal("deriveSignalTriggers() = nil error, want a rejection for missing every")
	}
}

func TestDeriveSignalTriggers_NoConfig_EmptyMap(t *testing.T) {
	sources := []SignalSource{{Connector: "slack/mentions", Service: "slack-api", Every: "10m"}}
	got, err := deriveSignalTriggers(sources)
	if err != nil {
		t.Fatalf("deriveSignalTriggers error = %v", err)
	}
	if got[0].Connector.Config == nil {
		t.Error("Connector.Config should default to an empty (non-nil) map when config: is omitted")
	}
	if len(got[0].Connector.Config) != 0 {
		t.Errorf("Connector.Config = %+v, want empty", got[0].Connector.Config)
	}
}

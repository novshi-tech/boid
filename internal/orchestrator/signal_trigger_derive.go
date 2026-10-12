package orchestrator

import (
	"fmt"
	"log/slog"
	"strings"
)

// signalTriggerRun is the fixed Run command every derived trigger carries:
// `Run: exec "$BOID_CONNECTOR_EXEC"`. The daemon never resolves
// BOID_CONNECTOR_EXEC itself at hydrate time — it is an env var StartExec
// sets on the connector job, substituted by the sandbox's own `sh -c` at
// runtime, the same "daemon does not interpret Run's contents" invariant
// every other trigger already has (Trigger.Run's own doc comment).
const signalTriggerRun = `exec "$BOID_CONNECTOR_EXEC"`

// signalTriggerNamePrefix names the derived trigger for source
// "<pack>/<connector>" as "signal:<pack>/<connector>" (one trigger per
// source).
const signalTriggerNamePrefix = "signal:"

func deriveSignalTriggers(sources []SignalSource) ([]Trigger, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(sources))
	triggers := make([]Trigger, 0, len(sources))
	for i, src := range sources {
		pack, connectorName, err := splitConnectorRef(src.Connector)
		if err != nil {
			return nil, fmt.Errorf("%s: signals.sources[%d]: %w", "workspace", i, err)
		}
		if src.Service == "" {
			return nil, fmt.Errorf("%s: signals.sources[%d] (%s): service must not be empty", "workspace", i, src.Connector)
		}
		if src.Every == "" {
			return nil, fmt.Errorf("%s: signals.sources[%d] (%s): every must not be empty", "workspace", i, src.Connector)
		}
		name := signalTriggerNamePrefix + src.Connector
		if seen[name] {
			return nil, fmt.Errorf("%s: signals.sources[%d]: duplicate source %q (would derive trigger name %q twice)", "workspace", i, src.Connector, name)
		}
		seen[name] = true

		config := src.Config
		if config == nil {
			config = map[string]any{}
		}

		triggers = append(triggers, Trigger{
			Name:  name,
			Every: src.Every,
			Run:   signalTriggerRun,
			Connector: &TriggerConnector{
				Pack:          pack,
				ConnectorName: connectorName,
				Service:       src.Service,
				Config:        config,
			},
		})
	}
	return triggers, nil
}

func warnSignalConnectorServicesNotEnabled(projectID, workspaceID string, triggers []Trigger, workspaceServices []string) {
	if len(triggers) == 0 {
		return
	}
	enabled := make(map[string]bool, len(workspaceServices))
	for _, s := range workspaceServices {
		enabled[s] = true
	}
	for _, trig := range triggers {
		if trig.Connector == nil {
			continue
		}
		if enabled[trig.Connector.Service] {
			continue
		}
		slog.Warn("signals.sources: declared service is not in this workspace's enabled services list",
			"project_id", projectID, "workspace_id", workspaceID,
			"trigger", trig.Name, "connector", trig.Connector.Pack+"/"+trig.Connector.ConnectorName,
			"service", trig.Connector.Service)
	}
}

// splitConnectorRef parses "<pack>/<connector>" into its two halves,
// rejecting anything else (empty, no slash, more than one slash, an empty
// half on either side of the slash).
func splitConnectorRef(ref string) (pack, connector string, err error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("connector: %q must be exactly \"<pack>/<connector>\"", ref)
	}
	return parts[0], parts[1], nil
}

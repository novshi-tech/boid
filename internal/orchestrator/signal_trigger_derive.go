package orchestrator

import (
	"fmt"
	"log/slog"
	"strings"
)

// signalTriggerRun is the fixed Run command every derived trigger carries
// (docs/plans/signal-ingest-detailed-design.md §5.1: `Run: exec
// "$BOID_CONNECTOR_EXEC"`). The daemon never resolves BOID_CONNECTOR_EXEC
// itself at hydrate time — it is an env var StartExec sets on the connector
// job (§5.2), substituted by the sandbox's own `sh -c` at runtime, the same
// "daemon does not interpret Run's contents" invariant every other trigger
// already has (Trigger.Run's own doc comment).
const signalTriggerRun = `exec "$BOID_CONNECTOR_EXEC"`

// signalTriggerNamePrefix names the derived trigger for source
// "<pack>/<connector>" as "signal:<pack>/<connector>" (§5.1: "source 1 件 =
// trigger 1 本").
const signalTriggerNamePrefix = "signal:"

// deriveSignalTriggers turns a project.yaml `signals.sources[]` list into
// the Trigger entries hydrate appends to meta.Triggers (docs/plans/
// signal-ingest-detailed-design.md §5.1, PR-5). Called from
// parseProjectMetaBytes (spec_loader.go) BEFORE ValidateTriggers runs over
// the combined (user + derived) Triggers slice — collision detection
// (a derived trigger colliding with a user-authored one, or two sources
// deriving the same name) is deliberately NOT duplicated here beyond the
// within-signals.sources duplicate check below; ValidateTriggers' existing
// "duplicate trigger name" check catches the user-vs-derived case for free
// once the two lists are concatenated (existing-mechanism reuse, matching
// this codebase's stated design philosophy of not inventing a second
// validation pass for the same invariant).
//
// Each returned Trigger has On=="" (schedule, the default — a connector
// trigger polls on its own schedule; it is not "on: signals", which is the
// UNRELATED judgment-side predicate §4 adds for a workspace's OWN
// triggers[] entries that scan the inbox).
//
// Returns an error (surfaces as a project.yaml load-time rejection, exactly
// like ValidateTriggers) for: a malformed `connector:` (not exactly
// "<pack>/<connector>"), an empty `service:`, an empty `every:`, or two
// sources that derive the SAME trigger name (duplicate connector
// declaration — §1's "source 1 件 = trigger 1 本" cannot hold for two
// sources naming the identical connector).
func deriveSignalTriggers(sources []SignalSource) ([]Trigger, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(sources))
	triggers := make([]Trigger, 0, len(sources))
	for i, src := range sources {
		pack, connectorName, err := splitConnectorRef(src.Connector)
		if err != nil {
			return nil, fmt.Errorf("project.yaml: signals.sources[%d]: %w", i, err)
		}
		if src.Service == "" {
			return nil, fmt.Errorf("project.yaml: signals.sources[%d] (%s): service must not be empty", i, src.Connector)
		}
		if src.Every == "" {
			return nil, fmt.Errorf("project.yaml: signals.sources[%d] (%s): every must not be empty", i, src.Connector)
		}
		name := signalTriggerNamePrefix + src.Connector
		if seen[name] {
			return nil, fmt.Errorf("project.yaml: signals.sources[%d]: duplicate source %q (would derive trigger name %q twice)", i, src.Connector, name)
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

// warnSignalConnectorServicesNotEnabled logs a warning (never an error —
// docs/plans/signal-ingest-detailed-design.md §5.2: "検証は警告のみ、エラーに
// しない") for each derived (Connector != nil) trigger whose declared
// service is not in the workspace's OWN enabled-services list. Called from
// ProjectStore.GetWithWorkspace — NOT project.yaml parse/load time (m1,
// PR-5 independent review finding): GetWithWorkspace hydrates on every
// call, and the trigger sweep loop calls it roughly once per project per
// sweep tick (1 minute, TriggerSweepResolution) via
// hydrateMetaForTriggers, so a persistent misconfiguration logs THIS
// warning roughly once a minute for as long as it remains unresolved, not
// just once at `boid project fetch` time. Accepted as-is (no throttling
// added) rather than introducing a new dedup-state mechanism to solve a
// cosmetic log-volume concern — see docs/ja/reference/project-yaml.md's
// "service の有効化チェック" section for the documented, accurate frequency.
//
// Deliberately checks against workspaceServices alone, NOT the
// floor-∪-workspace effective set config.APIGatewayServicesFloor
// contributes — internal/orchestrator has no visibility into the
// daemon-wide config floor (that lives in internal/config/internal/
// dispatcher, a layer this package must not import). This means a service
// enabled only via the floor will not trigger a (spurious) warning here —
// an acceptable, safe-direction imprecision for a warning-only check; §5.2
// does not require the check to be exhaustive, only present.
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

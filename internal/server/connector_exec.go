package server

import (
	"encoding/json"
	"fmt"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/integrationpack"
)

// connectorJobInput is resolveConnectorExec's output — everything
// sessionDispatcherAdapter.StartExec (wire.go) needs to add to the
// SessionJobInput it builds for a signal-derived trigger's connector job
// (docs/plans/signal-ingest-detailed-design.md §5.2).
type connectorJobInput struct {
	// Env carries the 4 connector env vars (§5.2 item 1):
	// BOID_SIGNAL_SERVICE / BOID_SIGNAL_CONNECTOR / BOID_SIGNAL_CONFIG /
	// BOID_CONNECTOR_EXEC. Merged into SessionJobInput.Env — no new
	// dispatcher-level field needed, reusing the existing pass-through.
	Env map[string]string
	// APIGatewayServices is always exactly [ref.Service] — copied onto
	// SessionJobInput.APIGatewayServices (§5.2 item 4).
	APIGatewayServices []string
}

// resolveConnectorExec resolves ref (a StartExecRequest.Connector's raw
// declaration) against packs (the daemon's loaded Pack registry,
// integrationpack.LoadPacks) into a connectorJobInput. Pure function — no
// server/DB/dispatch side effects — so sessionDispatcherAdapter.StartExec
// can fail the dispatch outright (surfacing through fireTrigger's existing
// fail-open retry) rather than launching a misconfigured connector job.
//
// Resolution order, each step a hard error naming the offending reference:
//  1. exactly one installed Pack version named ref.Pack must exist —
//     signals.sources[].connector carries no version (unlike config.yaml's
//     `uses: <pack>/<profile>@<version>`), so v0 refuses to guess when
//     more than one version is installed rather than silently picking one
//     ("先頭 slot を勝手に取るような縮退をしない", the same posture
//     integrationpack.DesugarService already applies to `uses:`).
//  2. ref.ConnectorName must name a connectors[] entry in that Pack's
//     manifest.
//  3. if the connector declares a configSchema, ref.Config must validate
//     against it (§6.2 item 4, Q19) — a v0 detail: unlike a Pack-level
//     manifest defect (§6.2 item 3's "起動エラー"), a config validation
//     failure here surfaces per-dispatch-attempt through the ordinary
//     StartExec error path (fireTrigger's fail-open retry), not as a
//     daemon-startup failure — this signals.sources[].config declaration
//     lives in ONE project's project.yaml, not the daemon-wide Pack
//     registry LoadPacks validates once at startup.
func resolveConnectorExec(packs []*integrationpack.Pack, ref apiwire.ConnectorRef) (connectorJobInput, error) {
	pack, err := findInstalledPack(packs, ref.Pack)
	if err != nil {
		return connectorJobInput{}, err
	}
	connector, ok := findConnector(pack, ref.ConnectorName)
	if !ok {
		return connectorJobInput{}, fmt.Errorf("integrationpack: connector %q: pack %q (installed version %s) declares no connector named %q", ref.Pack+"/"+ref.ConnectorName, pack.Name, pack.Version, ref.ConnectorName)
	}
	if connector.ConfigSchema != nil {
		if err := connector.ConfigSchema.Validate(ref.Config); err != nil {
			return connectorJobInput{}, fmt.Errorf("connector %q: config: %w", ref.Pack+"/"+ref.ConnectorName, err)
		}
	}

	configJSON, err := marshalConnectorConfig(ref.Config)
	if err != nil {
		return connectorJobInput{}, fmt.Errorf("connector %q: config: marshal: %w", ref.Pack+"/"+ref.ConnectorName, err)
	}

	// No bind mount: the daemon and every job container are launched from
	// the SAME base image (build/container/compose.yml 決定2 「daemon と
	// job runner が同じ versioned base イメージから起動」) — pack.Dir
	// (integrations.dir 配下の絶対パス, e.g. baked in at
	// /opt/boid/integrations/<pack>/<version> by build/container/Dockerfile)
	// already exists at the identical path inside the job container's own
	// filesystem, no transport needed. A bind mount would in fact be WRONG
	// here under the container backend's DooD (docker-out-of-docker) model:
	// the daemon hands BindMount.Source to the HOST's docker/podman (not the
	// daemon container's own filesystem) to resolve, so an image-baked,
	// daemon-container-only path can never be a valid bind source — nose,
	// 2026-08-27, after hitting exactly this ("mkdir /opt/boid: permission
	// denied") trying to dispatch a jira-cloud connector job.
	return connectorJobInput{
		Env: map[string]string{
			"BOID_SIGNAL_SERVICE":   ref.Service,
			"BOID_SIGNAL_CONNECTOR": ref.Pack + "/" + ref.ConnectorName,
			"BOID_SIGNAL_CONFIG":    configJSON,
			"BOID_CONNECTOR_EXEC":   pack.Dir + "/" + connector.Executable,
		},
		APIGatewayServices: []string{ref.Service},
	}, nil
}

// findInstalledPack returns the SOLE *Pack in packs named name, erroring on
// zero or more than one match — see resolveConnectorExec's doc comment for
// the "no version pin, no silent pick" rationale.
func findInstalledPack(packs []*integrationpack.Pack, name string) (*integrationpack.Pack, error) {
	var found []*integrationpack.Pack
	for _, p := range packs {
		if p.Name == name {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("integrationpack: pack %q is not installed (integrations.dir has no such pack directory)", name)
	case 1:
		return found[0], nil
	default:
		versions := make([]string, len(found))
		for i, p := range found {
			versions[i] = p.Version
		}
		return nil, fmt.Errorf("integrationpack: pack %q has %d installed versions (%v); signals.sources[].connector carries no version pin, so the choice is ambiguous — v0 requires exactly one installed version per pack name", name, len(found), versions)
	}
}

// findConnector looks up name within pack's manifest.
func findConnector(pack *integrationpack.Pack, name string) (integrationpack.Connector, bool) {
	for _, c := range pack.Manifest.Connectors {
		if c.Name == name {
			return c, true
		}
	}
	return integrationpack.Connector{}, false
}

// marshalConnectorConfig renders cfg as compact JSON for BOID_SIGNAL_CONFIG
// — nil/empty becomes "{}" (§5.2: "config の JSON"; the connector process
// json.loads()'s the value unconditionally, so it must never be empty-string
// or "null").
func marshalConnectorConfig(cfg map[string]any) (string, error) {
	if len(cfg) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

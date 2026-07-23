// Package reap implements the destroy logic behind the daemon-independent
// `boid reap` subcommand (cmd/reap.go — docs/plans/phase6-container-backend.md
// §PR6, §決定6). Its entire reason to exist is the plan doc's "deploy-level
// rollback reaper" contract: rollback from the compose (container backend)
// deploy to the host-daemon (userns backend) "旧デプロイ" must be able to
// stop/destroy every sibling job container the compose daemon created even
// when that daemon itself is down or unreachable — so Run talks to the
// docker engine directly, never through boid's own daemon API.
//
// §決定6 requires the UNION of two independent sources, neither sufficient
// alone:
//   - live docker resources carrying the boid.install_id=<id> label — the
//     primary job containers (and any volumes/networks) the daemon creates
//     directly via containerBackend (internal/dispatcher/container_backend.go).
//   - the per-job ledger files (<runtimesDir>/<jobID>/docker-resources.jsonl,
//     internal/sandbox/dockerproxy.Ledger) dockerproxy's sibling resources
//     are recorded into. Those carry NO boid label at all — the *client*
//     inside the sandbox (docker CLI, TestContainers, ...) creates them, not
//     boid — so the label query alone would never find them (§決定6: "従来
//     どおり per-job ledger... 管理").
package reap

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// LabelInstallID is the docker resource label reap's live query filters on.
// Duplicated (as a plain string, not an import) from
// internal/dispatcher.LabelInstallID deliberately: this package must stay
// importable from cmd (a daemon-independent CLI path — see cmd/reap.go)
// without pulling in internal/dispatcher's much larger dependency graph
// (DB, orchestrator, sqlite, ...), which would defeat "works even when the
// daemon — and everything it needs to build — is unable to start".
const LabelInstallID = "boid.install_id"

// dockerAPI is the narrow, package-owned subset of the docker Engine API
// Run needs — same "accept a small interface, not the SDK's big one" shape
// as internal/dispatcher's own dockerAPI (container_backend.go), and
// structurally satisfied by *github.com/moby/moby/client.Client with no
// wrapping required.
type dockerAPI interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)

	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)

	VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
}

// Report summarizes what Run destroyed (Destroyed*) or failed to
// (Errors — one formatted line per failure, "<type> <id>: <error>", so a
// CLI caller can print it directly without re-deriving context).
type Report struct {
	DestroyedContainers []string
	DestroyedNetworks   []string
	DestroyedVolumes    []string
	Errors              []string
}

// Empty reports whether Run found (and so, modulo failures, destroyed)
// nothing at all — the "already clean" case cmd/reap.go prints specially.
func (r Report) Empty() bool {
	return len(r.DestroyedContainers) == 0 && len(r.DestroyedNetworks) == 0 && len(r.DestroyedVolumes) == 0 && len(r.Errors) == 0
}

// Run destroys every docker resource belonging to installID: containers
// first (stop, best-effort, then force-remove — dependency order, matching
// dockerproxy.Reap and containerBackend.ReapOrphans), then networks
// (containers must be disconnected first), then volumes last (may still be
// referenced by a container mid-removal). Individual failures are recorded
// in Report.Errors and do not abort the rest — a single stuck container
// must not block reaping everything else (the deploy-level-rollback
// contract this exists for has no room for "reap partially worked, try
// again never").
//
// runtimesDir may be empty (ledger union skipped; the label query still
// runs) — see ledgerEntries.
func Run(ctx context.Context, api dockerAPI, installID, runtimesDir string) (Report, error) {
	containers, networks, volumes, err := unionResources(ctx, api, installID, runtimesDir)
	if err != nil {
		return Report{}, err
	}

	var report Report
	for _, id := range containers {
		if err := stopAndRemoveContainer(ctx, api, id); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("container %s: %v", id, err))
			continue
		}
		report.DestroyedContainers = append(report.DestroyedContainers, id)
	}
	for _, id := range networks {
		if _, err := api.NetworkRemove(ctx, id, client.NetworkRemoveOptions{}); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("network %s: %v", id, err))
			continue
		}
		report.DestroyedNetworks = append(report.DestroyedNetworks, id)
	}
	for _, id := range volumes {
		if _, err := api.VolumeRemove(ctx, id, client.VolumeRemoveOptions{Force: true}); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("volume %s: %v", id, err))
			continue
		}
		report.DestroyedVolumes = append(report.DestroyedVolumes, id)
	}
	return report, nil
}

// stopAndRemoveContainer mirrors dockerproxy.Reap's per-container sequence:
// a best-effort stop (ignored error — the container may already be
// stopped, or gone) followed by a forced remove.
func stopAndRemoveContainer(ctx context.Context, api dockerAPI, id string) error {
	_, _ = api.ContainerStop(ctx, id, client.ContainerStopOptions{})
	_, err := api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

// unionResources computes the deduped, sorted container/network/volume ID
// sets Run destroys: docker's own label-filtered list responses union'd
// with every resource ID recorded in an on-disk per-job ledger.
func unionResources(ctx context.Context, api dockerAPI, installID, runtimesDir string) (containers, networks, volumes []string, err error) {
	filters := client.Filters{}.Add("label", LabelInstallID+"="+installID)

	cSet := map[string]struct{}{}
	nSet := map[string]struct{}{}
	vSet := map[string]struct{}{}

	cList, err := api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reap: list containers: %w", err)
	}
	for _, c := range cList.Items {
		cSet[c.ID] = struct{}{}
	}

	nList, err := api.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reap: list networks: %w", err)
	}
	for _, n := range nList.Items {
		nSet[n.ID] = struct{}{}
	}

	vList, err := api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reap: list volumes: %w", err)
	}
	for _, v := range vList.Items {
		vSet[v.Name] = struct{}{}
	}

	for _, entry := range ledgerEntries(runtimesDir) {
		switch entry.Type {
		case "container":
			cSet[entry.ID] = struct{}{}
		case "network":
			nSet[entry.ID] = struct{}{}
		case "volume":
			vSet[entry.ID] = struct{}{}
		}
	}

	return sortedKeys(cSet), sortedKeys(nSet), sortedKeys(vSet), nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ledgerEntries reads every docker-resources.jsonl ledger directly under
// runtimesDir/*/ (dockerproxy.Ledger's fixed file name — see
// internal/dispatcher/runner.go's ledgerPath / internal/server/wire.go's
// matching cleanup-pass construction) and returns their concatenation.
// Returns nil when runtimesDir is empty. A missing or unreadable individual
// ledger is skipped, not fatal — a runtimes dir that GC has already
// partially cleaned must not block reaping everything else found.
func ledgerEntries(runtimesDir string) []dockerproxy.ResourceEntry {
	if runtimesDir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(runtimesDir, "*", "docker-resources.jsonl"))
	if err != nil {
		return nil
	}
	var out []dockerproxy.ResourceEntry
	for _, path := range matches {
		entries, err := dockerproxy.NewLedger(path).ReadAll()
		if err != nil {
			continue
		}
		out = append(out, entries...)
	}
	return out
}

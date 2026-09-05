// Package reap implements the destroy logic behind the daemon-independent
// `boid reap` subcommand (cmd/reap.go): it talks to the docker engine
// directly, never through boid's own daemon API, so it can stop/destroy
// every sibling job container an install created even when the daemon
// itself is down or unreachable.
//
// Run destroys the UNION of two independent sources, neither sufficient
// alone:
//   - live docker resources carrying the boid.install_id=<id> label — the
//     primary job containers (and any volumes/networks) the daemon creates
//     directly via containerBackend (internal/dispatcher/container_backend.go).
//   - the per-job ledger files (<runtimesDir>/<jobID>/docker-resources.jsonl,
//     internal/sandbox/dockerproxy.Ledger) dockerproxy's sibling resources
//     are recorded into. Those carry NO boid label at all — the *client*
//     inside the sandbox (docker CLI, TestContainers, ...) creates them, not
//     boid — so the label query alone would never find them.
//
// # Workspace HOME volumes are preserved by default
//
// Run takes a WorkspaceHomePolicy because "destroy everything this install
// created" and "do not destroy the workspace's credentials" are genuinely
// in tension. The contract: `boid reap` preserves persistent workspace HOME
// volumes by default, and `boid reap --include-workspace-homes` destroys
// them. containerBackend.ReapOrphans's startup pass ALWAYS preserves them —
// a daemon restart must never cost a workspace its harness authentication.
package reap

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// LabelInstallID is the docker resource label reap's live query filters on.
// An alias of internal/dockerres.LabelInstallID, which stays import-free so
// this package remains importable from cmd (a daemon-independent CLI path)
// without pulling in internal/dispatcher's much larger dependency graph (DB,
// orchestrator, sqlite, ...).
const LabelInstallID = dockerres.LabelInstallID

// WorkspaceHomePolicy tells Run what to do with the persistent workspace
// HOME volumes it enumerates. Its zero value is the fail-safe one: a caller
// that forgets to think about this preserves the data rather than
// destroying it.
type WorkspaceHomePolicy int

const (
	// PreserveWorkspaceHomes leaves every volume dockerres classifies as a
	// workspace HOME (boid-ws-home-*) alone, reporting it under
	// Report.SkippedVolumes instead. The default, and the only value
	// containerBackend.ReapOrphans's startup pass ever passes.
	PreserveWorkspaceHomes WorkspaceHomePolicy = iota

	// IncludeWorkspaceHomes destroys workspace HOME volumes along with
	// everything else — the explicit `boid reap --include-workspace-homes`
	// opt-in, for an operator who really is tearing the whole install down
	// and accepts losing every workspace's harness authentication and
	// toolchain with it.
	IncludeWorkspaceHomes
)

// preserves reports whether p protects volumeName from destruction.
func (p WorkspaceHomePolicy) preserves(volumeName string) bool {
	return p == PreserveWorkspaceHomes && dockerres.IsWorkspaceHomeVolumeName(volumeName)
}

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

// Report summarizes what Run destroyed (Destroyed*), deliberately left
// alone (SkippedVolumes), or failed to destroy (Errors — one formatted line
// per failure, "<type> <id>: <error>", so a CLI caller can print it directly
// without re-deriving context).
type Report struct {
	DestroyedContainers []string
	DestroyedNetworks   []string
	DestroyedVolumes    []string

	// SkippedVolumes lists the workspace HOME volumes Run enumerated but did
	// not destroy under PreserveWorkspaceHomes. Deliberately NOT part of
	// Empty(): a skip is not a destruction, so a run that only skipped
	// things is still "already clean" as far as Empty() is concerned.
	// cmd/reap.go prints these lines before consulting Empty() so the
	// preservation stays visible even in the "nothing to reap" case.
	SkippedVolumes []string

	Errors []string
}

// Empty reports whether Run found (and so, modulo failures, destroyed)
// nothing at all — the "already clean" case cmd/reap.go prints specially.
// SkippedVolumes is intentionally excluded; see its own doc comment.
func (r Report) Empty() bool {
	return len(r.DestroyedContainers) == 0 && len(r.DestroyedNetworks) == 0 && len(r.DestroyedVolumes) == 0 && len(r.Errors) == 0
}

// Run destroys every docker resource belonging to installID: containers
// first (stop, best-effort, then force-remove — dependency order, matching
// dockerproxy.Reap and containerBackend.ReapOrphans), then networks
// (containers must be disconnected first), then volumes last (may still be
// referenced by a container mid-removal). Individual failures are recorded
// in Report.Errors and do not abort the rest — a single stuck container
// must not block reaping everything else.
//
// A resource docker already reports as gone (404/NotFound, checked via
// errdefs.IsNotFound) is treated as successfully destroyed, not an error,
// since it may have already been removed some other way (typically a
// ledger-sourced entry — see unionResources). Every successfully destroyed
// id whose ledger entry can be traced (via unionResources' returned
// ledgerSource map) is drained from that ledger file afterward, so a
// subsequent reap run does not attempt it again.
//
// runtimesDir may be empty (ledger union skipped; the label query still
// runs) — see ledgerEntries.
//
// homes decides what happens to the persistent workspace HOME volumes among
// the enumerated set. The check is by NAME, not by label, because the
// ledger half of the union contributes bare ids with no labels attached at
// all — a label-based rule could not see them. Preserved volumes land in
// Report.SkippedVolumes.
func Run(ctx context.Context, api dockerAPI, installID, runtimesDir string, homes WorkspaceHomePolicy) (Report, error) {
	containers, networks, volumes, ledgerSource, err := unionResources(ctx, api, installID, runtimesDir)
	if err != nil {
		return Report{}, err
	}

	var report Report
	destroyed := map[string]bool{} // "<type>:<id>" for every id this run confirms gone

	for _, id := range containers {
		if err := stopAndRemoveContainer(ctx, api, id); err != nil && !errdefs.IsNotFound(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("container %s: %v", id, err))
			continue
		}
		report.DestroyedContainers = append(report.DestroyedContainers, id)
		destroyed["container:"+id] = true
	}
	for _, id := range networks {
		if _, err := api.NetworkRemove(ctx, id, client.NetworkRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("network %s: %v", id, err))
			continue
		}
		report.DestroyedNetworks = append(report.DestroyedNetworks, id)
		destroyed["network:"+id] = true
	}
	for _, id := range volumes {
		if homes.preserves(id) {
			// Deliberately NOT recorded in destroyed: this volume still
			// exists, so claiming it destroyed would make drainLedgers drop
			// a ledger entry for a live resource. A boid-ws-home-* entry
			// that reached a ledger stays there and is re-reported on every
			// subsequent run — bounded, since dockerproxy denies a sandboxed
			// docker client creating a volume in the reserved namespace at
			// all, so no new such entry can be recorded.
			report.SkippedVolumes = append(report.SkippedVolumes, id)
			continue
		}
		if _, err := api.VolumeRemove(ctx, id, client.VolumeRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("volume %s: %v", id, err))
			continue
		}
		report.DestroyedVolumes = append(report.DestroyedVolumes, id)
		destroyed["volume:"+id] = true
	}

	if err := drainLedgers(destroyed, ledgerSource); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("drain ledger: %v", err))
	}

	return report, nil
}

// drainLedgers rewrites every ledger file that contributed at least one
// destroyed entry, removing just those entries (any entry that failed to
// destroy — not in destroyed — is left in place for a future reap run).
// destroyed and ledgerSource are both keyed "<type>:<id>" — see Run and
// unionResources.
func drainLedgers(destroyed map[string]bool, ledgerSource map[string]string) error {
	if len(destroyed) == 0 {
		return nil
	}
	byPath := map[string]map[string]bool{}
	for key := range destroyed {
		path, ok := ledgerSource[key]
		if !ok {
			continue // this id came from the label query, not a ledger file
		}
		if byPath[path] == nil {
			byPath[path] = map[string]bool{}
		}
		byPath[path][key] = true
	}

	var errs []string
	for path, drop := range byPath {
		if err := drainLedgerFile(path, drop); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
	}
	return nil
}

// drainLedgerFile rewrites the ledger at path to contain only the entries
// whose "<type>:<id>" key is not in drop.
func drainLedgerFile(path string, drop map[string]bool) error {
	entries, err := dockerproxy.NewLedger(path).ReadAll()
	if err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}

	remaining := make([]dockerproxy.ResourceEntry, 0, len(entries))
	for _, e := range entries {
		if !drop[e.Type+":"+e.ID] {
			remaining = append(remaining, e)
		}
	}
	if len(remaining) == len(entries) {
		return nil // nothing in this file was drained
	}
	return dockerproxy.RewriteLedger(path, remaining)
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
//
// ledgerSource maps "<type>:<id>" -> the ledger file path that entry came
// from, for every id the ledger union contributed (an id docker's own
// label query alone found has no entry here) — Run's drain step
// (drainLedgers) uses it to know which file to rewrite once an id is
// confirmed destroyed.
//
// # Volumes are enumerated TWICE, once per label namespace
//
// The boid.install_id query cannot see a workspace HOME volume, by design:
// that label IS this query's filter, so such volumes record their install
// scope under boid.workspace_home_install_id instead (see
// internal/dockerres.LabelWorkspaceHomeInstallID). So the second query runs
// UNCONDITIONALLY, not just under IncludeWorkspaceHomes: enumeration is
// what feeds the skip report, and a policy that suppresses destruction must
// still be able to say what it suppressed. Run's homes.preserves check
// decides the fate of each name after enumeration, exactly as it already
// does for the ledger-sourced ids.
//
// Both queries treat an empty installID the same way — the filter term is
// emitted with an empty value ("<label>=") rather than dropped. A HOME
// volume created while the daemon had no install id carries no
// boid.workspace_home_install_id at all, so an install-scoped reap will not
// enumerate it — the fail-safe direction for a volume holding credentials.
func unionResources(ctx context.Context, api dockerAPI, installID, runtimesDir string) (containers, networks, volumes []string, ledgerSource map[string]string, err error) {
	filters := client.Filters{}.Add("label", LabelInstallID+"="+installID)
	homeFilters := client.Filters{}.Add("label", dockerres.LabelWorkspaceHomeInstallID+"="+installID)

	cSet := map[string]struct{}{}
	nSet := map[string]struct{}{}
	vSet := map[string]struct{}{}

	cList, err := api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reap: list containers: %w", err)
	}
	for _, c := range cList.Items {
		cSet[c.ID] = struct{}{}
	}

	nList, err := api.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reap: list networks: %w", err)
	}
	for _, n := range nList.Items {
		nSet[n.ID] = struct{}{}
	}

	vList, err := api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reap: list volumes: %w", err)
	}
	for _, v := range vList.Items {
		vSet[v.Name] = struct{}{}
	}

	homeList, err := api.VolumeList(ctx, client.VolumeListOptions{Filters: homeFilters})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reap: list workspace home volumes: %w", err)
	}
	for _, v := range homeList.Items {
		vSet[v.Name] = struct{}{}
	}

	ledgerSource = map[string]string{}
	for path, entries := range ledgerEntriesByPath(runtimesDir) {
		for _, entry := range entries {
			switch entry.Type {
			case "container":
				cSet[entry.ID] = struct{}{}
			case "network":
				nSet[entry.ID] = struct{}{}
			case "volume":
				vSet[entry.ID] = struct{}{}
			default:
				continue // e.g. "exec" — not a resource Run destroys directly
			}
			ledgerSource[entry.Type+":"+entry.ID] = path
		}
	}

	return sortedKeys(cSet), sortedKeys(nSet), sortedKeys(vSet), ledgerSource, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ledgerEntriesByPath reads every docker-resources.jsonl ledger directly
// under runtimesDir/*/ (dockerproxy.Ledger's fixed file name — see
// internal/dispatcher/runner.go's ledgerPath / internal/server/wire.go's
// matching cleanup-pass construction), keyed by the ledger file's own
// path so a caller can rewrite the right file later (unionResources'
// ledgerSource / drainLedgers). Returns nil when runtimesDir is empty. A
// missing or unreadable individual ledger is skipped, not fatal — a
// runtimes dir that GC has already partially cleaned must not block
// reaping everything else found.
func ledgerEntriesByPath(runtimesDir string) map[string][]dockerproxy.ResourceEntry {
	if runtimesDir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(runtimesDir, "*", "docker-resources.jsonl"))
	if err != nil {
		return nil
	}
	out := map[string][]dockerproxy.ResourceEntry{}
	for _, path := range matches {
		entries, err := dockerproxy.NewLedger(path).ReadAll()
		if err != nil || len(entries) == 0 {
			continue
		}
		out[path] = entries
	}
	return out
}

package reap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// matchesLabelFilters evaluates a docker list request's "label" filter term
// against a resource's labels the way dockerd itself does, so the fake below
// is a real stand-in for the engine rather than a rubber stamp.
//
// It exists because the fake used to IGNORE options.Filters and return every
// fixture unconditionally. That made every enumeration assertion in this file
// tautological — most damagingly TestRun_IncludeWorkspaceHomesDestroysThem,
// which "passed" while the flag it pins was a complete no-op against a real
// engine (the workspace HOME volume carries none of the labels the filter
// asked for, so dockerd would never have returned it). See unionResources.
//
// Semantics: the LABEL term specifically is AND, not OR. client.Filters' own
// doc comment describes the generic term rule ("a filter TERM is satisfied if
// ANY ONE of the values in its set is a match"), but dockerd does not apply
// that rule to labels — it routes them through MatchKVList, which requires
// EVERY specified value to match (`--filter label=a=1 --filter label=b=2`
// returns only resources carrying both). A bare "key" value matches on the
// label's mere presence; a "key=value" value matches on exact equality.
// Absence of the term matches everything.
//
// Modelling AND (rather than the OR an earlier revision of this fake used —
// codex review round 2, Minor 1) matters even though every production query
// in this package puts exactly one value in the term today, which makes the
// two rules indistinguishable right now: an OR fake is LOOSER than the
// engine, so the day someone adds a second value it would return rows dockerd
// would have withheld and quietly turn a real enumeration gap green. That is
// the exact failure this fake was tightened to prevent in the first place.
func matchesLabelFilters(filters client.Filters, labels map[string]string) bool {
	values := filters["label"]
	if len(values) == 0 {
		return true
	}
	for v := range values {
		key, want, hasValue := strings.Cut(v, "=")
		got, ok := labels[key]
		if !ok {
			return false
		}
		if hasValue && got != want {
			return false
		}
	}
	return true
}

// fakeDockerAPI is a minimal func-field fake, same shape as
// internal/dispatcher's fakeDockerAPI (container_backend_fake_test.go) but
// scoped to just the methods this package's dockerAPI interface needs.
type fakeDockerAPI struct {
	containers []container.Summary
	networks   []network.Summary
	volumes    []volume.Volume

	listFilters []client.Filters

	stoppedContainers  []string
	removedContainers  []string
	removedNetworks    []string
	removedVolumes     []string
	containerRemoveErr map[string]error
	networkRemoveErr   map[string]error
	volumeRemoveErr    map[string]error
}

var _ dockerAPI = (*fakeDockerAPI)(nil)

func (f *fakeDockerAPI) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.listFilters = append(f.listFilters, options.Filters)
	var items []container.Summary
	for _, c := range f.containers {
		if matchesLabelFilters(options.Filters, c.Labels) {
			items = append(items, c)
		}
	}
	return client.ContainerListResult{Items: items}, nil
}

func (f *fakeDockerAPI) ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.stoppedContainers = append(f.stoppedContainers, containerID)
	return client.ContainerStopResult{}, nil
}

func (f *fakeDockerAPI) ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removedContainers = append(f.removedContainers, containerID)
	if err, ok := f.containerRemoveErr[containerID]; ok {
		return client.ContainerRemoveResult{}, err
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDockerAPI) NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error) {
	f.listFilters = append(f.listFilters, options.Filters)
	var items []network.Summary
	for _, n := range f.networks {
		if matchesLabelFilters(options.Filters, n.Labels) {
			items = append(items, n)
		}
	}
	return client.NetworkListResult{Items: items}, nil
}

func (f *fakeDockerAPI) NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.removedNetworks = append(f.removedNetworks, networkID)
	if err, ok := f.networkRemoveErr[networkID]; ok {
		return client.NetworkRemoveResult{}, err
	}
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeDockerAPI) VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
	f.listFilters = append(f.listFilters, options.Filters)
	var items []volume.Volume
	for _, v := range f.volumes {
		if matchesLabelFilters(options.Filters, v.Labels) {
			items = append(items, v)
		}
	}
	return client.VolumeListResult{Items: items}, nil
}

func (f *fakeDockerAPI) VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.removedVolumes = append(f.removedVolumes, volumeID)
	if err, ok := f.volumeRemoveErr[volumeID]; ok {
		return client.VolumeRemoveResult{}, err
	}
	return client.VolumeRemoveResult{}, nil
}

// TestRun_LabelFilteredEnumerationAndDestroy pins §決定6's primary source:
// live docker resources carrying boid.install_id=<id> are listed via a
// label filter, then stopped/removed.
func TestRun_LabelFilteredEnumerationAndDestroy(t *testing.T) {
	api := &fakeDockerAPI{
		containers: []container.Summary{
			{ID: "c1", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
		networks: []network.Summary{
			{Network: network.Network{ID: "n1", Labels: map[string]string{LabelInstallID: "install-a"}}},
		},
		volumes: []volume.Volume{
			{Name: "v1", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", "", PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Four list calls, not three: volumes are enumerated TWICE, once per
	// label namespace (see unionResources — a workspace HOME volume carries
	// none of the job labels, by design, so a single install_id query can
	// never see it).
	if len(api.listFilters) != 4 {
		t.Fatalf("expected 4 filtered list calls (containers/networks/volumes×2), got %d", len(api.listFilters))
	}
	wantTerms := []string{
		"boid.install_id=install-a",
		"boid.install_id=install-a",
		"boid.install_id=install-a",
		"boid.workspace_home_install_id=install-a",
	}
	for i, f := range api.listFilters {
		if _, ok := f["label"][wantTerms[i]]; !ok {
			t.Errorf("list call %d: filters = %v, want label %s", i, f, wantTerms[i])
		}
	}

	if len(api.stoppedContainers) != 1 || api.stoppedContainers[0] != "c1" {
		t.Errorf("stoppedContainers = %v, want [c1]", api.stoppedContainers)
	}
	if len(api.removedContainers) != 1 || api.removedContainers[0] != "c1" {
		t.Errorf("removedContainers = %v, want [c1]", api.removedContainers)
	}
	if len(api.removedNetworks) != 1 || api.removedNetworks[0] != "n1" {
		t.Errorf("removedNetworks = %v, want [n1]", api.removedNetworks)
	}
	if len(api.removedVolumes) != 1 || api.removedVolumes[0] != "v1" {
		t.Errorf("removedVolumes = %v, want [v1]", api.removedVolumes)
	}

	if len(report.DestroyedContainers) != 1 || report.DestroyedContainers[0] != "c1" {
		t.Errorf("report.DestroyedContainers = %v, want [c1]", report.DestroyedContainers)
	}
	if len(report.DestroyedNetworks) != 1 || report.DestroyedNetworks[0] != "n1" {
		t.Errorf("report.DestroyedNetworks = %v, want [n1]", report.DestroyedNetworks)
	}
	if len(report.DestroyedVolumes) != 1 || report.DestroyedVolumes[0] != "v1" {
		t.Errorf("report.DestroyedVolumes = %v, want [v1]", report.DestroyedVolumes)
	}
	if len(report.Errors) != 0 {
		t.Errorf("report.Errors = %v, want none", report.Errors)
	}
	if report.Empty() {
		t.Error("report.Empty() = true, want false (resources were destroyed)")
	}
}

// TestRun_LedgerUnion pins §決定6's second source: resources recorded in a
// per-job docker-resources.jsonl ledger (no boid label at all — dockerproxy
// sibling resources) must be reaped too, even when the label query finds
// nothing.
func TestRun_LedgerUnion(t *testing.T) {
	runtimesDir := t.TempDir()
	jobDir := filepath.Join(runtimesDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledger := dockerproxy.NewLedger(filepath.Join(jobDir, "docker-resources.jsonl"))
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "container", ID: "sibling-c1"}); err != nil {
		t.Fatalf("append container entry: %v", err)
	}
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "sibling-v1"}); err != nil {
		t.Fatalf("append volume entry: %v", err)
	}

	api := &fakeDockerAPI{} // label query finds nothing

	report, err := Run(context.Background(), api, "install-a", runtimesDir, PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(report.DestroyedContainers) != 1 || report.DestroyedContainers[0] != "sibling-c1" {
		t.Errorf("report.DestroyedContainers = %v, want [sibling-c1]", report.DestroyedContainers)
	}
	if len(report.DestroyedVolumes) != 1 || report.DestroyedVolumes[0] != "sibling-v1" {
		t.Errorf("report.DestroyedVolumes = %v, want [sibling-v1]", report.DestroyedVolumes)
	}
}

// TestRun_UnionDedupesOverlap covers the case where the SAME resource
// appears in both sources (label query and ledger) — it must be
// stopped/removed exactly once, not twice.
func TestRun_UnionDedupesOverlap(t *testing.T) {
	runtimesDir := t.TempDir()
	jobDir := filepath.Join(runtimesDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledger := dockerproxy.NewLedger(filepath.Join(jobDir, "docker-resources.jsonl"))
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "container", ID: "c1"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	api := &fakeDockerAPI{
		containers: []container.Summary{
			{ID: "c1", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", runtimesDir, PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.removedContainers) != 1 {
		t.Errorf("removedContainers = %v, want exactly one removal of c1", api.removedContainers)
	}
	if len(report.DestroyedContainers) != 1 {
		t.Errorf("report.DestroyedContainers = %v, want exactly [c1]", report.DestroyedContainers)
	}
}

// TestRun_EmptyRuntimesDirSkipsLedgerUnion covers Run("", "") — the ledger
// glob must not error or panic on an empty runtimesDir.
func TestRun_EmptyRuntimesDirSkipsLedgerUnion(t *testing.T) {
	api := &fakeDockerAPI{}
	report, err := Run(context.Background(), api, "install-a", "", PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Empty() {
		t.Errorf("report = %+v, want empty", report)
	}
}

// TestRun_ContinuesAfterIndividualFailure pins the "one stuck resource must
// not block the rest" contract: a container remove failure is recorded in
// Report.Errors but does not prevent the network/volume from being
// destroyed.
func TestRun_ContinuesAfterIndividualFailure(t *testing.T) {
	api := &fakeDockerAPI{
		containers: []container.Summary{
			{ID: "c-stuck", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
		networks: []network.Summary{
			{Network: network.Network{ID: "n1", Labels: map[string]string{LabelInstallID: "install-a"}}},
		},
		containerRemoveErr: map[string]error{
			"c-stuck": context.DeadlineExceeded,
		},
	}

	report, err := Run(context.Background(), api, "install-a", "", PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.DestroyedContainers) != 0 {
		t.Errorf("DestroyedContainers = %v, want none (remove failed)", report.DestroyedContainers)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("report.Errors = %v, want exactly one entry", report.Errors)
	}
	if len(report.DestroyedNetworks) != 1 || report.DestroyedNetworks[0] != "n1" {
		t.Errorf("DestroyedNetworks = %v, want [n1] (must proceed past the container failure)", report.DestroyedNetworks)
	}
}

// notFoundErr is a minimal error implementing the containerd/errdefs
// "NotFound" marker interface (a bare NotFound() method) — the same shape
// github.com/moby/moby/client wraps a real 404 response into. Used to
// simulate "docker already reports this resource as gone" without a real
// docker daemon.
type notFoundErr struct{}

func (notFoundErr) Error() string { return "not found" }
func (notFoundErr) NotFound()     {}

// TestRun_NotFoundDuringDestroy_TreatedAsSuccess_AndDrainsLedger pins
// Major 8 (PR6 codex review): a resource this run's own union found (via
// the ledger, the only source that can carry an id docker's live label
// query no longer reports) but that docker's remove call reports 404/
// NotFound for must be treated as destroyed, not an error — and its
// ledger entry must be drained so a second Run over the same
// install/runtimesDir does not even attempt it again (the pre-fix
// behavior: report an error for the same already-gone id on every single
// run, forever).
func TestRun_NotFoundDuringDestroy_TreatedAsSuccess_AndDrainsLedger(t *testing.T) {
	runtimesDir := t.TempDir()
	jobDir := filepath.Join(runtimesDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledgerPath := filepath.Join(jobDir, "docker-resources.jsonl")
	ledger := dockerproxy.NewLedger(ledgerPath)
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "container", ID: "sibling-c1"}); err != nil {
		t.Fatalf("append container entry: %v", err)
	}
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "sibling-v1"}); err != nil {
		t.Fatalf("append volume entry: %v", err)
	}

	api := &fakeDockerAPI{
		containerRemoveErr: map[string]error{"sibling-c1": notFoundErr{}},
		volumeRemoveErr:    map[string]error{"sibling-v1": notFoundErr{}},
	}

	report, err := Run(context.Background(), api, "install-a", runtimesDir, PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Errorf("report.Errors = %v, want none (404/NotFound must be treated as success)", report.Errors)
	}
	if len(report.DestroyedContainers) != 1 || report.DestroyedContainers[0] != "sibling-c1" {
		t.Errorf("report.DestroyedContainers = %v, want [sibling-c1]", report.DestroyedContainers)
	}
	if len(report.DestroyedVolumes) != 1 || report.DestroyedVolumes[0] != "sibling-v1" {
		t.Errorf("report.DestroyedVolumes = %v, want [sibling-v1]", report.DestroyedVolumes)
	}

	remaining, err := dockerproxy.NewLedger(ledgerPath).ReadAll()
	if err != nil {
		t.Fatalf("read ledger after drain: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("ledger after drain = %v, want empty (both destroyed entries removed)", remaining)
	}

	// Second run over the same (now-drained) ledger and the same
	// (still-empty) label query: nothing left to find, so nothing to
	// destroy, and definitely no repeated error for sibling-c1/sibling-v1.
	api2 := &fakeDockerAPI{}
	report2, err := Run(context.Background(), api2, "install-a", runtimesDir, PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !report2.Empty() {
		t.Errorf("second Run report = %+v, want empty (ledger already drained)", report2)
	}
}

// TestRun_DrainLeavesUndestroyedEntriesInPlace covers the flip side of the
// drain step: an entry whose remove call fails with a real (non-NotFound)
// error must stay in the ledger for a future reap run, not be dropped
// alongside its successfully destroyed sibling.
func TestRun_DrainLeavesUndestroyedEntriesInPlace(t *testing.T) {
	runtimesDir := t.TempDir()
	jobDir := filepath.Join(runtimesDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledgerPath := filepath.Join(jobDir, "docker-resources.jsonl")
	ledger := dockerproxy.NewLedger(ledgerPath)
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "container", ID: "ok-c1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "container", ID: "stuck-c2"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	api := &fakeDockerAPI{
		containerRemoveErr: map[string]error{"stuck-c2": context.DeadlineExceeded},
	}

	report, err := Run(context.Background(), api, "install-a", runtimesDir, PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("report.Errors = %v, want exactly one entry (stuck-c2)", report.Errors)
	}

	remaining, err := dockerproxy.NewLedger(ledgerPath).ReadAll()
	if err != nil {
		t.Fatalf("read ledger after drain: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != "stuck-c2" {
		t.Errorf("ledger after drain = %v, want exactly [stuck-c2] (only the destroyed entry is dropped)", remaining)
	}
}

// TestRun_PreservesWorkspaceHomeVolumes pins the containment PR1 exists for
// (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 1): the
// default WorkspaceHomePolicy must leave a boid-ws-home-* volume alone and
// report it under SkippedVolumes, from BOTH enumeration sources — the
// install_id label query and the per-job ledger. The ledger half is the one
// a label-based rule could never cover, since ledger entries carry no
// labels at all.
func TestRun_PreservesWorkspaceHomeVolumes(t *testing.T) {
	runtimesDir := t.TempDir()
	jobDir := filepath.Join(runtimesDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledgerPath := filepath.Join(jobDir, "docker-resources.jsonl")
	ledger := dockerproxy.NewLedger(ledgerPath)
	// A workspace HOME volume that reached the ledger (the pre-PR1 hole:
	// a sandboxed docker client could `docker volume create` the
	// deterministic name and have this reaper delete the real volume).
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "boid-ws-home-install1-default"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "scratch-vol"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	api := &fakeDockerAPI{
		volumes: []volume.Volume{
			// Label-query source. The labels are EXACTLY the ones
			// containerBackend.ensureNamedVolumes puts on a workspace HOME
			// volume — deliberately none of the job labels, so only the
			// workspace-home-scoped query can enumerate it.
			{Name: "boid-ws-home-install1-boid", Labels: map[string]string{
				dockerres.LabelWorkspaceHome:          "boid",
				dockerres.LabelWorkspaceHomeInstallID: "install-a",
			}},
			{Name: "job-vol-1", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", runtimesDir, PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, name := range api.removedVolumes {
		if strings.HasPrefix(name, "boid-ws-home-") {
			t.Errorf("VolumeRemove called for workspace HOME volume %q; want it preserved", name)
		}
	}
	if len(api.removedVolumes) != 2 {
		t.Errorf("removedVolumes = %v, want exactly the two non-HOME volumes", api.removedVolumes)
	}

	wantSkipped := map[string]bool{
		"boid-ws-home-install1-default": true,
		"boid-ws-home-install1-boid":    true,
	}
	if len(report.SkippedVolumes) != len(wantSkipped) {
		t.Fatalf("report.SkippedVolumes = %v, want %v", report.SkippedVolumes, wantSkipped)
	}
	for _, name := range report.SkippedVolumes {
		if !wantSkipped[name] {
			t.Errorf("report.SkippedVolumes contains unexpected %q", name)
		}
	}
	for _, name := range report.DestroyedVolumes {
		if wantSkipped[name] {
			t.Errorf("report.DestroyedVolumes contains skipped volume %q", name)
		}
	}
}

// TestRun_SkippedWorkspaceHomeStaysInLedger pins the drain decision PR1
// makes explicitly: a skipped volume was NOT destroyed, so it must not be
// drained from the ledger (draining it would claim a destruction that never
// happened). See Run's doc comment for why the resulting "reported on every
// run" repetition is bounded rather than permanent.
func TestRun_SkippedWorkspaceHomeStaysInLedger(t *testing.T) {
	runtimesDir := t.TempDir()
	jobDir := filepath.Join(runtimesDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledgerPath := filepath.Join(jobDir, "docker-resources.jsonl")
	ledger := dockerproxy.NewLedger(ledgerPath)
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "boid-ws-home-install1-default"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "scratch-vol"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	api := &fakeDockerAPI{}
	if _, err := Run(context.Background(), api, "install-a", runtimesDir, PreserveWorkspaceHomes); err != nil {
		t.Fatalf("Run: %v", err)
	}

	remaining, err := dockerproxy.NewLedger(ledgerPath).ReadAll()
	if err != nil {
		t.Fatalf("read ledger after drain: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != "boid-ws-home-install1-default" {
		t.Errorf("ledger after drain = %v, want exactly [boid-ws-home-install1-default]", remaining)
	}
}

// TestRun_IncludeWorkspaceHomesDestroysThem pins the escape hatch `boid reap
// --include-workspace-homes` exposes: with the opt-in policy, a workspace
// HOME volume is destroyed like any other and reported under
// DestroyedVolumes (never SkippedVolumes).
func TestRun_IncludeWorkspaceHomesDestroysThem(t *testing.T) {
	api := &fakeDockerAPI{
		volumes: []volume.Volume{
			{Name: "boid-ws-home-install1-default", Labels: map[string]string{
				dockerres.LabelWorkspaceHome:          "default",
				dockerres.LabelWorkspaceHomeInstallID: "install-a",
			}},
			{Name: "job-vol-1", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", "", IncludeWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.removedVolumes) != 2 {
		t.Errorf("removedVolumes = %v, want both volumes removed", api.removedVolumes)
	}
	if len(report.SkippedVolumes) != 0 {
		t.Errorf("report.SkippedVolumes = %v, want none", report.SkippedVolumes)
	}
	if len(report.DestroyedVolumes) != 2 {
		t.Errorf("report.DestroyedVolumes = %v, want both", report.DestroyedVolumes)
	}
}

// TestRun_EnumeratesWorkspaceHomeVolumesByTheirOwnLabel pins the second
// volume enumeration source unionResources needs (PR1 codex review Blocker
// 1). A workspace HOME volume deliberately carries NONE of the job labels —
// boid.install_id in particular, because that is precisely the filter Run's
// live query uses (docs/plans/workspace-home-volume-persistence.md 論点 a
// 経路 1 / the ensureNamedVolumes label table). The consequence is that the
// install_id query alone cannot see it AT ALL, which breaks the declared
// contract in both directions:
//
//   - PreserveWorkspaceHomes: not enumerated means the skip branch is never
//     reached, so Report.SkippedVolumes stays empty and "skip した volume は
//     必ず出力する" silently becomes a lie.
//   - IncludeWorkspaceHomes: not enumerated means VolumeRemove is never
//     called, so `boid reap --include-workspace-homes` is a total no-op.
//
// Both halves are asserted here against a fixture labeled exactly the way
// containerBackend.ensureNamedVolumes labels a real one.
func TestRun_EnumeratesWorkspaceHomeVolumesByTheirOwnLabel(t *testing.T) {
	newAPI := func() *fakeDockerAPI {
		return &fakeDockerAPI{
			volumes: []volume.Volume{
				{Name: "boid-ws-home-install1-default", Labels: map[string]string{
					dockerres.LabelWorkspaceHome:          "default",
					dockerres.LabelWorkspaceHomeInstallID: "install-a",
				}},
			},
		}
	}

	t.Run("preserve reports it as skipped", func(t *testing.T) {
		api := newAPI()
		report, err := Run(context.Background(), api, "install-a", "", PreserveWorkspaceHomes)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(api.removedVolumes) != 0 {
			t.Errorf("removedVolumes = %v, want none", api.removedVolumes)
		}
		if len(report.SkippedVolumes) != 1 || report.SkippedVolumes[0] != "boid-ws-home-install1-default" {
			t.Errorf("report.SkippedVolumes = %v, want [boid-ws-home-install1-default] — a volume that is never enumerated can never be reported as skipped", report.SkippedVolumes)
		}
	})

	t.Run("include destroys it", func(t *testing.T) {
		api := newAPI()
		report, err := Run(context.Background(), api, "install-a", "", IncludeWorkspaceHomes)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(api.removedVolumes) != 1 || api.removedVolumes[0] != "boid-ws-home-install1-default" {
			t.Errorf("removedVolumes = %v, want [boid-ws-home-install1-default] — --include-workspace-homes must not be a no-op", api.removedVolumes)
		}
		if len(report.DestroyedVolumes) != 1 {
			t.Errorf("report.DestroyedVolumes = %v, want the HOME volume", report.DestroyedVolumes)
		}
	})
}

// TestRun_DoesNotEnumerateAnotherInstallsWorkspaceHome pins the scoping half
// of the fix: the extra query is filtered on
// boid.workspace_home_install_id=<installID>, so another installation sharing
// the same docker engine keeps its credentials even under
// --include-workspace-homes. (Without the filter — e.g. enumerating every
// boid.workspace_home-labeled volume — this reap would be a cross-install
// data-loss bug, which is the same class of mistake reapOwnsLabels exists to
// prevent for containers.)
// TestRun_WorkspaceHomeMatchingBothVolumeQueriesIsDestroyedOnce pins the
// dedup contract of unionResources' two-query volume enumeration (codex
// review round 2, Minor 2). A volume carrying BOTH boid.install_id and
// boid.workspace_home_install_id is returned by each VolumeList, and the
// vSet map — not the loops — is what stops it being removed twice.
//
// The doubly-labeled volume is not a shape boid emits today
// (ensureNamedVolumes applies one label set or the other, never both), which
// is exactly why it needs a test: nothing else would notice if a later
// refactor replaced vSet with an append.
func TestRun_WorkspaceHomeMatchingBothVolumeQueriesIsDestroyedOnce(t *testing.T) {
	api := &fakeDockerAPI{
		volumes: []volume.Volume{
			{Name: "boid-ws-home-install1-default", Labels: map[string]string{
				LabelInstallID:                        "install-a",
				dockerres.LabelWorkspaceHome:          "default",
				dockerres.LabelWorkspaceHomeInstallID: "install-a",
			}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", "", IncludeWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.removedVolumes) != 1 || api.removedVolumes[0] != "boid-ws-home-install1-default" {
		t.Errorf("removedVolumes = %v, want exactly one entry — both VolumeList queries return this volume and unionResources must dedupe them", api.removedVolumes)
	}
	if len(report.DestroyedVolumes) != 1 {
		t.Errorf("report.DestroyedVolumes = %v, want a single entry", report.DestroyedVolumes)
	}
}

func TestRun_DoesNotEnumerateAnotherInstallsWorkspaceHome(t *testing.T) {
	api := &fakeDockerAPI{
		volumes: []volume.Volume{
			{Name: "boid-ws-home-other123-default", Labels: map[string]string{
				dockerres.LabelWorkspaceHome:          "default",
				dockerres.LabelWorkspaceHomeInstallID: "install-b",
			}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", "", IncludeWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.removedVolumes) != 0 {
		t.Errorf("removedVolumes = %v, want none (another install's HOME volume)", api.removedVolumes)
	}
	if !report.Empty() {
		t.Errorf("report = %+v, want empty", report)
	}
}

// TestRun_WorkspaceNetworkIsStillReaped guards against over-containment:
// the reserved "boid-ws-" prefix covers the per-workspace docker NETWORK
// names too, but those are recreated on demand by ensureWorkspaceNetwork, so
// reap must still destroy them. Only the narrower "boid-ws-home-" volume
// namespace is preserved (internal/dockerres's package doc).
func TestRun_WorkspaceNetworkIsStillReaped(t *testing.T) {
	api := &fakeDockerAPI{
		networks: []network.Summary{
			{Network: network.Network{ID: "boid-ws-install1-default", Labels: map[string]string{LabelInstallID: "install-a"}}},
		},
		volumes: []volume.Volume{
			// Reserved namespace but NOT a HOME volume, so it carries the
			// ordinary job labels and the ordinary install_id query finds it.
			{Name: "boid-ws-install1-default", Labels: map[string]string{LabelInstallID: "install-a"}},
		},
	}

	report, err := Run(context.Background(), api, "install-a", "", PreserveWorkspaceHomes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.removedNetworks) != 1 {
		t.Errorf("removedNetworks = %v, want the workspace network destroyed", api.removedNetworks)
	}
	if len(api.removedVolumes) != 1 {
		t.Errorf("removedVolumes = %v, want the non-HOME reserved volume destroyed", api.removedVolumes)
	}
	if len(report.SkippedVolumes) != 0 {
		t.Errorf("report.SkippedVolumes = %v, want none", report.SkippedVolumes)
	}
}

// TestReport_EmptyIgnoresSkippedVolumes pins the deliberate decision to
// leave Report.Empty()'s definition alone: a skip is not a destruction, so
// a run that only skipped things is still "nothing to reap" as far as
// Empty() is concerned (cmd/reap.go prints the skip lines before consulting
// it).
func TestReport_EmptyIgnoresSkippedVolumes(t *testing.T) {
	r := Report{SkippedVolumes: []string{"boid-ws-home-install1-default"}}
	if !r.Empty() {
		t.Error("Report.Empty() = false for a skip-only report, want true (skip is not destroy)")
	}
}

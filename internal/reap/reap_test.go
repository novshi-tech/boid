package reap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

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
	return client.ContainerListResult{Items: f.containers}, nil
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
	return client.NetworkListResult{Items: f.networks}, nil
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
	return client.VolumeListResult{Items: f.volumes}, nil
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
			{Network: network.Network{ID: "n1"}},
		},
		volumes: []volume.Volume{
			{Name: "v1"},
		},
	}

	report, err := Run(context.Background(), api, "install-a", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(api.listFilters) != 3 {
		t.Fatalf("expected 3 filtered list calls (containers/networks/volumes), got %d", len(api.listFilters))
	}
	for i, f := range api.listFilters {
		if _, ok := f["label"]["boid.install_id=install-a"]; !ok {
			t.Errorf("list call %d: filters = %v, want label boid.install_id=install-a", i, f)
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

	report, err := Run(context.Background(), api, "install-a", runtimesDir)
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

	report, err := Run(context.Background(), api, "install-a", runtimesDir)
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
	report, err := Run(context.Background(), api, "install-a", "")
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
			{Network: network.Network{ID: "n1"}},
		},
		containerRemoveErr: map[string]error{
			"c-stuck": context.DeadlineExceeded,
		},
	}

	report, err := Run(context.Background(), api, "install-a", "")
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

	report, err := Run(context.Background(), api, "install-a", runtimesDir)
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
	report2, err := Run(context.Background(), api2, "install-a", runtimesDir)
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

	report, err := Run(context.Background(), api, "install-a", runtimesDir)
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

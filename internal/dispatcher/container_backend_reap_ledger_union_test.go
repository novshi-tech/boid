package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// TestContainerBackend_ReapOrphans_ClearsLedgerSourcedSiblingWithNoLabel
// pins the second half of [Major 6, PR7 codex review]: a docker-enabled
// job's SIBLING resources — created by the *client* inside the sandbox
// (docker CLI, TestContainers, ...) via the per-job dockerproxy, never by
// this backend directly — carry NO boid label at all, so ReapOrphans' own
// label-filtered ContainerList sweep can never find them. They are only
// discoverable via the per-job docker-resources.jsonl ledger file under
// RuntimeDir (§決定8's ledger contract, the same one `boid reap`
// (internal/reap.Run) already reads). ReapOrphans must additionally run
// that ledger-union pass so startup reap catches these too, not just the
// primary (labeled) job containers.
func TestContainerBackend_ReapOrphans_ClearsLedgerSourcedSiblingWithNoLabel(t *testing.T) {
	runtimeDir := t.TempDir()

	// Seed a per-job ledger recording one sibling container with NO boid
	// label — exactly what dockerproxy's own ledger tracking produces for a
	// client-created (not backend-created) resource.
	jobDir := filepath.Join(runtimeDir, "orphaned-job-id")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledger := dockerproxy.NewLedger(filepath.Join(jobDir, "docker-resources.jsonl"))
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "container", ID: "sibling-container-no-label"}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	api := &fakeDockerAPI{
		// No boid.job_id-labeled containers at all — the primary-container
		// sweep must find nothing; only the ledger union should surface the
		// sibling.
		ContainerListFunc: func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
			return client.ContainerListResult{Items: []container.Summary{}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})

	report, err := be.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(report.ReapedJobIDs) != 0 || len(report.FailedJobIDs) != 0 {
		t.Errorf("ReapReport = %+v, want empty (the ledger-sourced sibling has no job_id to report against — reap.Run's own contract, not ReapOrphans')", report)
	}

	found := false
	for _, id := range api.removeIDs {
		if id == "sibling-container-no-label" {
			found = true
		}
	}
	if !found {
		t.Errorf("ContainerRemove calls = %v, want the ledger-sourced sibling %q included (Major 6's ledger-union pass)", api.removeIDs, "sibling-container-no-label")
	}

	// The ledger entry should have been drained once its resource was
	// confirmed destroyed (reap.Run's own drainLedgers contract), so a
	// subsequent reap does not attempt it again. A FRESH Ledger instance is
	// used here deliberately: the original `ledger` variable above has
	// already cached its entries in memory (Ledger.ensureLoaded's
	// documented behavior) from the Append call, so re-reading through it
	// would only replay that stale cache rather than observing what
	// reap.Run's own RewriteLedger actually wrote to disk.
	remaining, err := dockerproxy.NewLedger(filepath.Join(jobDir, "docker-resources.jsonl")).ReadAll()
	if err != nil {
		t.Fatalf("ledger.ReadAll after reap: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("ledger entries after reap = %v, want empty (destroyed entry drained)", remaining)
	}
}

// TestContainerBackend_ReapOrphans_LedgerUnionSkippedWhenRuntimeDirUnset
// pins the companion non-regression: RuntimeDir unset (every pre-Major-6
// test/caller) must not attempt the ledger-union pass at all — there is no
// runtimeDir to glob ledger files under, and this must not panic or error.
func TestContainerBackend_ReapOrphans_LedgerUnionSkippedWhenRuntimeDirUnset(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
}

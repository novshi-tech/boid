package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestEgressPortStore_SaveThenLoad(t *testing.T) {
	store := orchestrator.NewEgressPortStore(newTestDB(t))

	if _, ok, err := store.LoadPort("default"); err != nil || ok {
		t.Fatalf("LoadPort on an empty store = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	if err := store.SavePort("default", 30412); err != nil {
		t.Fatalf("SavePort: %v", err)
	}

	port, ok, err := store.LoadPort("default")
	if err != nil {
		t.Fatalf("LoadPort: %v", err)
	}
	if !ok || port != 30412 {
		t.Errorf("LoadPort = (%d, %v), want (30412, true)", port, ok)
	}
}

// TestEgressPortStore_SaveIsIdempotentPerKey pins that re-saving a key
// REPLACES its port rather than erroring on the primary key — the
// reallocation path (persisted port taken, a new one bound) writes through
// this method, and failing there would leave the store pointing at a port
// nothing is listening on.
func TestEgressPortStore_SaveIsIdempotentPerKey(t *testing.T) {
	store := orchestrator.NewEgressPortStore(newTestDB(t))

	if err := store.SavePort("default", 30412); err != nil {
		t.Fatalf("first SavePort: %v", err)
	}
	if err := store.SavePort("default", 30988); err != nil {
		t.Fatalf("second SavePort: %v", err)
	}

	port, ok, err := store.LoadPort("default")
	if err != nil || !ok {
		t.Fatalf("LoadPort = (_, %v, %v)", ok, err)
	}
	if port != 30988 {
		t.Errorf("LoadPort = %d, want the most recently saved 30988", port)
	}
}

// TestEgressPortStore_PortStolenByAnotherKey: `port` carries a UNIQUE
// constraint, so handing key B a port already recorded for key A must not
// fail the save — the Go side has already bound the listener by then, and
// the only correct resolution is for the newcomer to win and the stale
// record to go. (A can only have lost the port by no longer holding it.)
func TestEgressPortStore_PortStolenByAnotherKey(t *testing.T) {
	store := orchestrator.NewEgressPortStore(newTestDB(t))

	if err := store.SavePort("ws-a", 30412); err != nil {
		t.Fatalf("SavePort ws-a: %v", err)
	}
	if err := store.SavePort("ws-b", 30412); err != nil {
		t.Fatalf("SavePort ws-b on a port already recorded for ws-a: %v", err)
	}

	port, ok, err := store.LoadPort("ws-b")
	if err != nil || !ok || port != 30412 {
		t.Errorf("LoadPort(ws-b) = (%d, %v, %v), want (30412, true, nil)", port, ok, err)
	}
	if _, ok, err := store.LoadPort("ws-a"); err != nil || ok {
		t.Errorf("LoadPort(ws-a) = (_, %v, %v), want (_, false, nil) — the stale record must be gone", ok, err)
	}
}

// TestEgressPortStore_ReservedKey pins that the no-workspace listener key
// stores fine. It is not a valid workspace slug (ValidWorkspaceSlug forbids
// "_"), which is exactly why workspace_egress_port has no foreign key to
// workspaces(slug) — see the migration's own comment.
func TestEgressPortStore_ReservedKey(t *testing.T) {
	store := orchestrator.NewEgressPortStore(newTestDB(t))

	const key = "__no_workspace__"
	if err := store.SavePort(key, 30777); err != nil {
		t.Fatalf("SavePort(%q): %v", key, err)
	}
	port, ok, err := store.LoadPort(key)
	if err != nil || !ok || port != 30777 {
		t.Errorf("LoadPort(%q) = (%d, %v, %v), want (30777, true, nil)", key, port, ok, err)
	}
}

// TestEgressPortStore_RejectsEmptyKey: an unkeyed row would be silently
// shared by every caller that forgot to pass a key.
func TestEgressPortStore_RejectsEmptyKey(t *testing.T) {
	store := orchestrator.NewEgressPortStore(newTestDB(t))

	if err := store.SavePort("", 30412); err == nil {
		t.Error("SavePort with an empty key = nil, want an error")
	}
}

// TestEgressPortStore_ReservedPorts covers the lookup the allocator uses to
// avoid handing one key a port another key is already reserving.
func TestEgressPortStore_ReservedPorts(t *testing.T) {
	store := orchestrator.NewEgressPortStore(newTestDB(t))

	if got, err := store.ReservedPorts(); err != nil || len(got) != 0 {
		t.Fatalf("ReservedPorts on an empty store = (%v, %v), want (empty, nil)", got, err)
	}

	if err := store.SavePort("ws-a", 30412); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePort("__no_workspace__", 30777); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReservedPorts()
	if err != nil {
		t.Fatalf("ReservedPorts: %v", err)
	}
	want := map[int]string{30412: "ws-a", 30777: "__no_workspace__"}
	if len(got) != len(want) {
		t.Fatalf("ReservedPorts = %v, want %v", got, want)
	}
	for port, key := range want {
		if got[port] != key {
			t.Errorf("ReservedPorts[%d] = %q, want %q", port, got[port], key)
		}
	}
}

// TestWorkspaceRepository_Remove_ReleasesEgressPort: the reservation table
// has no FK to workspaces(slug) (its key space includes the non-slug
// "__no_workspace__"), so nothing cascades. A removed workspace's row left
// behind would hold a band slot forever, and the allocator now treats
// reservations as off-limits — orphans would slowly eat the band.
func TestWorkspaceRepository_Remove_ReleasesEgressPort(t *testing.T) {
	conn := newTestDB(t)
	repo := orchestrator.NewWorkspaceRepository(conn)
	store := orchestrator.NewEgressPortStore(conn)

	if err := repo.Create("doomed", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SavePort("doomed", 30412); err != nil {
		t.Fatalf("SavePort: %v", err)
	}

	if err := repo.Remove("doomed"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok, err := store.LoadPort("doomed"); err != nil || ok {
		t.Errorf("LoadPort after Remove = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}

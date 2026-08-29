package sandbox

import "testing"

// isBlockingBoidRequest is an explicit per-op allowlist, and an op dropped from
// it still blocks — with nothing left to unblock it when the sandbox goes away
// (wiring-seams.md #31). This pins the exact set, so a silent removal fails
// here rather than becoming a leak nobody sees.
//
// It cannot notice a NEW blocking op on its own — nothing in the type system
// says which executor paths block — so adding one means adding it here, to
// isBlockingBoidRequest, and writing its conn-close twin test.
func TestBroker_BlockingOps_ListIsPinned(t *testing.T) {
	blocking := map[BoidOp]bool{
		BoidOpTaskAsk:  true,
		BoidOpTaskWait: true,
	}

	// Every op declared in protocol.go must answer the predicate the way this
	// table says. Listing the non-blocking ones by exclusion keeps the check
	// honest without enumerating all ~35 of them.
	for _, op := range []BoidOp{
		BoidOpTaskAsk, BoidOpTaskWait,
		BoidOpJobDone, BoidOpTaskCreate, BoidOpTaskGet, BoidOpTaskUpdate,
		BoidOpTaskReopen, BoidOpTaskList, BoidOpTaskNotify, BoidOpTaskAnswer,
		BoidOpTaskDelete, BoidOpActionList, BoidOpSignalList, BoidOpSignalAck,
	} {
		req := &ExecRequest{Boid: &BoidRequest{Op: op}}
		if got := isBlockingBoidRequest(req); got != blocking[op] {
			t.Errorf("isBlockingBoidRequest(%q) = %v, want %v", op, got, blocking[op])
		}
	}

	// A request with no boid payload is never blocking — handleConn calls this
	// on every request, including plain command execs.
	if isBlockingBoidRequest(&ExecRequest{}) {
		t.Error("a request with no boid payload must not be treated as blocking")
	}
}

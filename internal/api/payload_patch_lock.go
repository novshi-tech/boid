package api

import (
	"hash/fnv"
	"sync"
)

// payloadPatchLockShards is a fixed-size stripe count for
// payloadPatchLockFor. A fixed, small shard count trades a small,
// statistically rare cross-task lock collision for zero cleanup complexity
// — unlike a plain map[string]*sync.Mutex (which would grow forever over a
// long-running daemon's lifetime as new task ids appear and are never
// released), a fixed array of mutexes is allocated once and never grows.
const payloadPatchLockShards = 64

var payloadPatchLocks [payloadPatchLockShards]sync.Mutex

// payloadPatchLockFor returns the mutex TaskAppService.UpdateTaskPayloadPatch
// uses to serialize its read-modify-write critical section
// (GetTask -> MergePayloadPatch -> UpdateTask) for a given task id, closing
// the lost-update race a bare RMW sequence has under concurrent callers —
// e.g. two hooks of the same readonly task's parallel dispatch round, each
// patching a different sub-key, where the second full-row UPDATE would
// otherwise silently discard the first's write (Phase 5b PR7 codex review
// Blocker 2, wiring-seams.md #17).
//
// Deliberately much narrower in scope and duration than the retired
// per-task branch lock (memory: khi-supervisor-branch-lock-headline-block),
// which held for a task's entire executing lifetime and caused
// head-of-line blocking across unrelated tasks queued behind it: this
// lock's critical section is only the handful of DB calls inside a single
// UpdateTaskPayloadPatch call (milliseconds), so even an unlucky shard
// collision between two unrelated tasks is a brief queueing delay, never a
// multi-hour stall.
//
// Scope: this closes concurrent UpdateTaskPayloadPatch calls racing against
// EACH OTHER. It does not (and is not intended to) serialize against every
// other task-row writer in the codebase (ApplyAction, NotifyTask, the
// existing --payload-file UpdateTask, ...) — none of those share this lock,
// so a race between UpdateTaskPayloadPatch and one of them remains
// possible in principle. Closing that fully general problem would need
// optimistic-concurrency versioning on every task write path, which is a
// separate, much larger effort outside this fix's scope.
func payloadPatchLockFor(taskID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return &payloadPatchLocks[h.Sum32()%payloadPatchLockShards]
}

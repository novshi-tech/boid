package dispatcher

import (
	"sync/atomic"
	"time"
)

// atomicDuration holds a duration that production only ever reads and a test
// swaps for a shorter one.
//
// It exists because the plain-var version of that pattern is a real data race
// in this package, not a theoretical one [CI race detector, PR8]. The test
// helpers that shrink these bounds all reasoned the same way — "no test here
// calls t.Parallel(), so a process-wide swap is safe" — and that reasoning is
// wrong for a reason parallelism does not cover: the readers include goroutines
// that OUTLIVE the test that started them. containerSession.waitLoop is the one
// CI caught, reading a cleanup bound from a previous test's session while the
// next test was assigning to it.
//
// Sequential tests are therefore not enough; what would be needed is that every
// goroutine a test starts has been joined before the next test writes, which is
// not a property any of these tests establish. Making the value atomic removes
// the requirement instead of documenting it.
type atomicDuration struct{ nanos atomic.Int64 }

// newAtomicDuration returns a bound initialized to d.
func newAtomicDuration(d time.Duration) *atomicDuration {
	a := &atomicDuration{}
	a.nanos.Store(int64(d))
	return a
}

// Get reads the current bound.
func (a *atomicDuration) Get() time.Duration {
	return time.Duration(a.nanos.Load())
}

// Set installs d and returns the value it replaced, so a test can restore it
// in a t.Cleanup without a separate read.
func (a *atomicDuration) Set(d time.Duration) (previous time.Duration) {
	return time.Duration(a.nanos.Swap(int64(d)))
}

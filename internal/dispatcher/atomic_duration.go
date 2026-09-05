package dispatcher

import (
	"sync/atomic"
	"time"
)

// atomicDuration holds a duration that production only ever reads and a test
// swaps for a shorter one; atomic because a goroutine reading the bound can
// outlive the test that started it, racing the next test's write.
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

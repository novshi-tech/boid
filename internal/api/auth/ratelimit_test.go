package auth

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowUnderLimit(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	for i := range rateLimitMax {
		if !rl.Allow("1.2.3.4") {
			t.Errorf("Allow attempt %d: got false, want true", i+1)
		}
	}
}

func TestRateLimiter_BlockOnExceed(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	for range rateLimitMax {
		rl.Allow("1.2.3.4")
	}

	if rl.Allow("1.2.3.4") {
		t.Error("Allow on 6th attempt: got true, want false")
	}
}

func TestRateLimiter_UnlockAfterLockDuration(t *testing.T) {
	base := time.Now()
	current := base
	rl := NewRateLimiter(func() time.Time { return current })

	for range rateLimitMax {
		rl.Allow("1.2.3.4")
	}
	// 6th attempt triggers lock
	rl.Allow("1.2.3.4")

	// Still locked just before the lock expires
	current = base.Add(rateLimitLockTime - time.Second)
	if rl.Allow("1.2.3.4") {
		t.Error("Allow before lock expiry: got true, want false")
	}

	// Allowed after lock duration passes
	current = base.Add(rateLimitLockTime + time.Second)
	if !rl.Allow("1.2.3.4") {
		t.Error("Allow after lock expiry: got false, want true")
	}
}

func TestRateLimiter_IndependentIPs(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	for range rateLimitMax {
		rl.Allow("10.0.0.1")
	}
	// 10.0.0.1 is now at limit; 10.0.0.2 should still be allowed
	if !rl.Allow("10.0.0.2") {
		t.Error("Allow for different IP: got false, want true")
	}
}

func TestRateLimiter_AllowedNoSideEffects(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	// Calling Allowed many times must not cause a lock.
	for range rateLimitMax * 10 {
		if !rl.Allowed("1.2.3.4") {
			t.Error("Allowed should be true before any RecordFailure")
		}
	}
}

func TestRateLimiter_RecordFailureLocks(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	for range rateLimitMax {
		rl.RecordFailure("1.2.3.4")
	}

	if rl.Allowed("1.2.3.4") {
		t.Error("Allowed after rateLimitMax failures: got true, want false")
	}
}

func TestRateLimiter_RecordFailureUnderLimitStaysAllowed(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	for range rateLimitMax - 1 {
		rl.RecordFailure("1.2.3.4")
	}

	if !rl.Allowed("1.2.3.4") {
		t.Error("Allowed under threshold: got false, want true")
	}
}

func TestRateLimiter_RecordFailureIndependentIPs(t *testing.T) {
	now := time.Now()
	rl := NewRateLimiter(func() time.Time { return now })

	for range rateLimitMax {
		rl.RecordFailure("10.0.0.1")
	}

	if rl.Allowed("10.0.0.1") {
		t.Error("Allowed for locked IP: got true, want false")
	}
	if !rl.Allowed("10.0.0.2") {
		t.Error("Allowed for independent IP: got false, want true")
	}
}

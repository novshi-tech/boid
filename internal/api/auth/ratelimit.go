package auth

import (
	"sync"
	"time"
)

const (
	rateLimitWindow   = 5 * time.Minute
	rateLimitMax      = 5
	rateLimitLockTime = 15 * time.Minute
)

type ipState struct {
	attempts    []time.Time
	lockedUntil time.Time
}

type RateLimiter struct {
	now   func() time.Time
	mu    sync.Mutex
	state map[string]*ipState
}

func NewRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		now:   now,
		state: make(map[string]*ipState),
	}
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	s := rl.state[ip]
	if s == nil {
		s = &ipState{}
		rl.state[ip] = s
	}

	if now.Before(s.lockedUntil) {
		return false
	}

	cutoff := now.Add(-rateLimitWindow)
	var recent []time.Time
	for _, t := range s.attempts {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	s.attempts = recent

	if len(s.attempts) >= rateLimitMax {
		s.lockedUntil = now.Add(rateLimitLockTime)
		return false
	}

	s.attempts = append(s.attempts, now)
	return true
}

// Allowed reports whether ip is currently not rate-limited (read-only, no side effects).
func (rl *RateLimiter) Allowed(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	s := rl.state[ip]
	if s == nil {
		return true
	}
	return !rl.now().Before(s.lockedUntil)
}

// RecordFailure records a failed attempt for ip and locks it if the threshold is exceeded.
func (rl *RateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	s := rl.state[ip]
	if s == nil {
		s = &ipState{}
		rl.state[ip] = s
	}

	cutoff := now.Add(-rateLimitWindow)
	var recent []time.Time
	for _, t := range s.attempts {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	s.attempts = append(recent, now)

	if len(s.attempts) >= rateLimitMax {
		s.lockedUntil = now.Add(rateLimitLockTime)
	}
}

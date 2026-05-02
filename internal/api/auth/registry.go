package auth

import "sync"

// ConnectionRegistry maps device IDs to the set of revoke channels for active
// long-lived connections (SSE / WebSocket). Calling RevokeDevice closes all
// channels registered for that device, causing handlers to unblock and return.
type ConnectionRegistry struct {
	mu      sync.Mutex
	entries map[string]map[chan struct{}]struct{}
}

func NewConnectionRegistry() *ConnectionRegistry {
	return &ConnectionRegistry{
		entries: make(map[string]map[chan struct{}]struct{}),
	}
}

// Register records a long-lived connection for deviceID and returns a channel
// that is closed when the device is revoked, plus a release function that must
// be deferred by the caller to clean up when the connection ends normally.
func (r *ConnectionRegistry) Register(deviceID string) (<-chan struct{}, func()) {
	ch := make(chan struct{})

	r.mu.Lock()
	if r.entries[deviceID] == nil {
		r.entries[deviceID] = make(map[chan struct{}]struct{})
	}
	r.entries[deviceID][ch] = struct{}{}
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if chs := r.entries[deviceID]; chs != nil {
			delete(chs, ch)
			if len(chs) == 0 {
				delete(r.entries, deviceID)
			}
		}
	}
	return ch, release
}

// RevokeDevice closes all revoke channels registered for deviceID.
func (r *ConnectionRegistry) RevokeDevice(deviceID string) {
	r.mu.Lock()
	chs := r.entries[deviceID]
	delete(r.entries, deviceID)
	r.mu.Unlock()

	for ch := range chs {
		close(ch)
	}
}

// RevokeAll closes all revoke channels for every registered device.
func (r *ConnectionRegistry) RevokeAll() {
	r.mu.Lock()
	all := r.entries
	r.entries = make(map[string]map[chan struct{}]struct{})
	r.mu.Unlock()

	for _, chs := range all {
		for ch := range chs {
			close(ch)
		}
	}
}

//go:build linux

package dispatcher

// RuntimeSubscriber subscribes to live output of a running job identified by jobID.
type RuntimeSubscriber interface {
	Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool)
}

// Subscribe implements RuntimeSubscriber for Runner. It resolves jobID to a
// runtimeID via the jobs table, then delegates to LocalRuntime if the runtime
// supports live streaming.
func (r *Runner) Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool) {
	var runtimeID string
	if err := r.DB.QueryRow(`SELECT runtime_id FROM jobs WHERE id = ?`, jobID).Scan(&runtimeID); err != nil || runtimeID == "" {
		return nil, nil, func() {}, false
	}
	sub, capable := r.Runtime.(interface {
		SubscribeRuntime(string) ([]byte, <-chan []byte, func(), bool)
	})
	if !capable {
		return nil, nil, func() {}, false
	}
	return sub.SubscribeRuntime(runtimeID)
}

// SubscribeRuntime subscribes to live output of the session identified by runtimeID.
// Returns the current transcript snapshot, a channel of subsequent chunks,
// a cancel function to unsubscribe, and whether live streaming is available.
func (r *LocalRuntime) SubscribeRuntime(runtimeID string) ([]byte, <-chan []byte, func(), bool) {
	session, err := r.session(runtimeID)
	if err != nil {
		return nil, nil, func() {}, false
	}
	snap, subID, sessionCh, running := session.subscribe()
	if !running {
		return snap, nil, func() {}, false
	}
	return snap, sessionCh, func() { session.unsubscribe(subID) }, true
}

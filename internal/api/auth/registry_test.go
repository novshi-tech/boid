package auth

import (
	"sync"
	"testing"
	"time"
)

func TestConnectionRegistry_RevokeDevice_ClosesChannel(t *testing.T) {
	reg := NewConnectionRegistry()
	ch, release := reg.Register("dev-1")
	defer release()

	go func() {
		time.Sleep(10 * time.Millisecond)
		reg.RevokeDevice("dev-1")
	}()

	select {
	case <-ch:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after RevokeDevice")
	}
}

func TestConnectionRegistry_Release_PreventsClose(t *testing.T) {
	reg := NewConnectionRegistry()
	ch, release := reg.Register("dev-2")

	// Normal exit: release before revoke.
	release()

	// RevokeDevice for the same ID should be a no-op (no entries to close).
	reg.RevokeDevice("dev-2")

	// Channel must NOT be closed (it was already released normally).
	select {
	case <-ch:
		t.Fatal("channel was closed after release + revoke; expected it to remain open")
	case <-time.After(50 * time.Millisecond):
		// correct: channel still open
	}
}

func TestConnectionRegistry_MultipleConnections(t *testing.T) {
	reg := NewConnectionRegistry()

	const n = 5
	chs := make([]<-chan struct{}, n)
	releases := make([]func(), n)
	for i := range n {
		chs[i], releases[i] = reg.Register("dev-3")
	}
	defer func() {
		for _, r := range releases {
			r()
		}
	}()

	reg.RevokeDevice("dev-3")

	for i, ch := range chs {
		select {
		case <-ch:
			// expected
		case <-time.After(2 * time.Second):
			t.Fatalf("channel[%d] not closed after RevokeDevice", i)
		}
	}
}

func TestConnectionRegistry_RevokeAll(t *testing.T) {
	reg := NewConnectionRegistry()

	ch1, release1 := reg.Register("devA")
	ch2, release2 := reg.Register("devB")
	defer release1()
	defer release2()

	reg.RevokeAll()

	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
			// expected
		case <-time.After(2 * time.Second):
			t.Fatalf("channel[%d] not closed after RevokeAll", i)
		}
	}
}

func TestConnectionRegistry_ConcurrentAccess(t *testing.T) {
	reg := NewConnectionRegistry()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			deviceID := "device"
			ch, release := reg.Register(deviceID)
			// Randomly either release or wait for revoke.
			if i%2 == 0 {
				release()
			} else {
				select {
				case <-ch:
				case <-time.After(500 * time.Millisecond):
					release()
				}
			}
		}(i)
	}

	time.Sleep(20 * time.Millisecond)
	reg.RevokeAll()
	wg.Wait()
}

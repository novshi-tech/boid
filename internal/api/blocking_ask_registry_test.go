package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBlockingAskRegistry_WaitNotifyRoundTrip(t *testing.T) {
	r := NewBlockingAskRegistry()
	if err := r.Register("task-1", "q-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer r.Cancel("q-1")

	got := make(chan string, 1)
	go func() {
		ans, err := r.Wait(context.Background(), "q-1")
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		got <- ans
	}()

	// Give the waiter a moment to enter the select, then deliver.
	if !waitUntil(func() bool { return r.Has("q-1") }) {
		t.Fatal("question never registered")
	}
	if !r.Notify("q-1", "yes do it") {
		t.Fatal("Notify returned false for a registered question")
	}

	select {
	case ans := <-got:
		if ans != "yes do it" {
			t.Errorf("answer = %q, want %q", ans, "yes do it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return the delivered answer")
	}
}

// Notify before Wait must still deliver: the channel is buffered (cap 1) so the
// answer is parked until the waiter selects it. This guards the (sub-millisecond
// but real) window between the task transitioning to awaiting and Wait entering
// its select.
func TestBlockingAskRegistry_NotifyBeforeWait(t *testing.T) {
	r := NewBlockingAskRegistry()
	if err := r.Register("task-1", "q-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer r.Cancel("q-1")

	if !r.Notify("q-1", "early answer") {
		t.Fatal("Notify returned false")
	}
	ans, err := r.Wait(context.Background(), "q-1")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if ans != "early answer" {
		t.Errorf("answer = %q, want early answer", ans)
	}
}

func TestBlockingAskRegistry_RegisterB1_SecondPendingFails(t *testing.T) {
	r := NewBlockingAskRegistry()
	if err := r.Register("task-1", "q-1"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	defer r.Cancel("q-1")

	err := r.Register("task-1", "q-2")
	if !errors.Is(err, ErrAskPending) {
		t.Fatalf("second Register err = %v, want ErrAskPending", err)
	}

	// A different task is unaffected.
	if err := r.Register("task-2", "q-3"); err != nil {
		t.Fatalf("Register for other task: %v", err)
	}
	r.Cancel("q-3")
}

// After Cancel the B1 slot is freed so the same task can ask again.
func TestBlockingAskRegistry_CancelFreesTaskSlot(t *testing.T) {
	r := NewBlockingAskRegistry()
	if err := r.Register("task-1", "q-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Cancel("q-1")
	if r.Has("q-1") {
		t.Fatal("question still registered after Cancel")
	}
	if err := r.Register("task-1", "q-2"); err != nil {
		t.Fatalf("re-Register after Cancel: %v", err)
	}
	r.Cancel("q-2")
}

func TestBlockingAskRegistry_WaitCancelledByContext(t *testing.T) {
	r := NewBlockingAskRegistry()
	if err := r.Register("task-1", "q-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer r.Cancel("q-1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Wait(ctx, "q-1")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after context cancellation")
	}
}

func TestBlockingAskRegistry_NotifyUnknownReturnsFalse(t *testing.T) {
	r := NewBlockingAskRegistry()
	if r.Notify("nope", "x") {
		t.Error("Notify for unknown qid should return false")
	}
}

// Exercises concurrent Register/Wait/Notify/Cancel so `go test -race` can catch
// any unsynchronised map access.
func TestBlockingAskRegistry_RaceConcurrentAccess(t *testing.T) {
	r := NewBlockingAskRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			taskID := "task-" + itoa(i)
			qid := "q-" + itoa(i)
			if err := r.Register(taskID, qid); err != nil {
				return
			}
			go r.Notify(qid, "answer")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = r.Wait(ctx, qid)
			r.Cancel(qid)
		}(i)
	}
	wg.Wait()
}

func waitUntil(cond func() bool) bool {
	for i := 0; i < 200; i++ {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

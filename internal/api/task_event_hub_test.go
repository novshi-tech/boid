package api

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTaskEventHub_SubscribeAndBroadcast(t *testing.T) {
	hub := NewTaskEventHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := hub.Subscribe(ctx, "task-1")

	ev := TaskEvent{Kind: "action", Payload: "hello"}
	hub.Broadcast("task-1", ev)

	select {
	case got := <-ch:
		if got.Kind != ev.Kind {
			t.Fatalf("expected kind %q, got %q", ev.Kind, got.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestTaskEventHub_CancelUnsubscribes(t *testing.T) {
	hub := NewTaskEventHub()
	ctx, cancel := context.WithCancel(context.Background())

	ch := hub.Subscribe(ctx, "task-1")
	cancel()

	// チャネルがクローズされるまで待つ
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel, got value")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: channel not closed after ctx cancel")
	}

	// subscriber map から除去されていること
	hub.mu.Lock()
	_, exists := hub.subs["task-1"]
	hub.mu.Unlock()
	if exists {
		t.Fatal("subscriber map entry should be removed after cancel")
	}
}

func TestTaskEventHub_DifferentTaskIDs(t *testing.T) {
	hub := NewTaskEventHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1 := hub.Subscribe(ctx, "task-1")
	ch2 := hub.Subscribe(ctx, "task-2")

	hub.Broadcast("task-1", TaskEvent{Kind: "action"})

	select {
	case <-ch1:
	case <-time.After(time.Second):
		t.Fatal("task-1 subscriber should receive event")
	}

	select {
	case got := <-ch2:
		t.Fatalf("task-2 subscriber should not receive task-1 event, got %v", got)
	case <-time.After(50 * time.Millisecond):
		// OK: 届かないことを確認
	}
}

func TestTaskEventHub_SlowSubscriberDoesNotBlockFast(t *testing.T) {
	hub := NewTaskEventHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// バッファを使い切らせる slow subscriber
	slowCtx, slowCancel := context.WithCancel(context.Background())
	defer slowCancel()
	_ = hub.Subscribe(slowCtx, "task-1")

	fast := hub.Subscribe(ctx, "task-1")

	// バッファ (16) を超える数の Broadcast
	const n = 32
	for i := 0; i < n; i++ {
		hub.Broadcast("task-1", TaskEvent{Kind: "action"})
	}

	// fast subscriber はバッファ分を受信できる（ブロックされていない）
	received := 0
	for {
		select {
		case <-fast:
			received++
		default:
			goto done
		}
	}
done:
	if received == 0 {
		t.Fatal("fast subscriber received nothing")
	}
}

func TestTaskEventHub_ConcurrentRace(t *testing.T) {
	hub := NewTaskEventHub()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			ch := hub.Subscribe(ctx, "task-race")
			go func() {
				for range ch {
				}
			}()
			for j := 0; j < 20; j++ {
				hub.Broadcast("task-race", TaskEvent{Kind: "action"})
			}
			cancel()
		}()
	}
	wg.Wait()
}

package orchestrator_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeGCStore is a test double for GCStore.
type fakeGCStore struct {
	callCount atomic.Int64
	alwaysFail bool
}

func (f *fakeGCStore) GC(_ time.Duration, _ bool) (*orchestrator.GCResult, error) {
	f.callCount.Add(1)
	if f.alwaysFail {
		return nil, errors.New("fake gc error")
	}
	return &orchestrator.GCResult{Tasks: 1}, nil
}

func TestGCLoop_CallsGCMultipleTimes(t *testing.T) {
	fake := &fakeGCStore{}
	loop := &orchestrator.GCLoop{
		Store:        fake,
		Interval:     10 * time.Millisecond,
		OlderThan:    24 * time.Hour,
		InitialDelay: 1 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go loop.Run(ctx)

	// 150ms あれば初期遅延 1ms + 10ms*10 程度のティックが発生するため余裕がある
	time.Sleep(150 * time.Millisecond)
	cancel()

	got := fake.callCount.Load()
	if got < 3 {
		t.Fatalf("expected at least 3 GC calls, got %d", got)
	}
}

func TestGCLoop_CtxCancelExits(t *testing.T) {
	fake := &fakeGCStore{}
	loop := &orchestrator.GCLoop{
		Store:        fake,
		Interval:     10 * time.Millisecond,
		OlderThan:    0,
		InitialDelay: 1 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Run が正しく終了した
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel within 2s")
	}
}

func TestGCLoop_CtxCancelDuringInitialDelay(t *testing.T) {
	fake := &fakeGCStore{}
	loop := &orchestrator.GCLoop{
		Store:        fake,
		Interval:     10 * time.Millisecond,
		OlderThan:    0,
		InitialDelay: 10 * time.Second, // 長い遅延でキャンセルをテスト
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// InitialDelay 中のキャンセルで即座に終了した
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit during InitialDelay after ctx cancel")
	}
}

func TestGCLoop_ErrorContinues(t *testing.T) {
	fake := &fakeGCStore{alwaysFail: true}
	loop := &orchestrator.GCLoop{
		Store:        fake,
		Interval:     10 * time.Millisecond,
		OlderThan:    0,
		InitialDelay: 1 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go loop.Run(ctx)

	// エラーが返り続けてもループが継続することを確認
	time.Sleep(80 * time.Millisecond)
	cancel()

	got := fake.callCount.Load()
	if got < 3 {
		t.Fatalf("expected at least 3 GC calls even with errors, got %d", got)
	}
}

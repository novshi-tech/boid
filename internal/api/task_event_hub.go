package api

import (
	"context"
	"sync"
)

// TaskEvent はタスクに関連するイベントを表す。
type TaskEvent struct {
	Kind    string // "action" / "job" / "fired_event" etc.
	Payload any
}

// subEntry は 1 subscriber を表す。mu で送信とクローズを排他する。
type subEntry struct {
	ch   chan TaskEvent
	mu   sync.Mutex
	dead bool
}

// tryClose はチャネルを一度だけクローズする。
func (s *subEntry) tryClose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dead {
		s.dead = true
		close(s.ch)
	}
}

// trySend は dead でない場合のみ非ブロッキングで送信する。
func (s *subEntry) trySend(ev TaskEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dead {
		select {
		case s.ch <- ev:
		default:
		}
	}
}

// TaskEventHub は taskID ごとに複数の subscriber へイベントを配送する pub-sub ハブ。
type TaskEventHub struct {
	mu   sync.Mutex
	subs map[string]map[uint64]*subEntry
	seq  uint64
}

// NewTaskEventHub は新しい TaskEventHub を返す。
func NewTaskEventHub() *TaskEventHub {
	return &TaskEventHub{
		subs: make(map[string]map[uint64]*subEntry),
	}
}

// Subscribe は taskID のイベントを受け取るチャネルを返す。
// ctx がキャンセルされると、チャネルはクローズされ内部からも除去される。
func (h *TaskEventHub) Subscribe(ctx context.Context, taskID string) <-chan TaskEvent {
	entry := &subEntry{ch: make(chan TaskEvent, 16)}

	h.mu.Lock()
	id := h.seq
	h.seq++
	if h.subs[taskID] == nil {
		h.subs[taskID] = make(map[uint64]*subEntry)
	}
	h.subs[taskID][id] = entry
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs[taskID], id)
		if len(h.subs[taskID]) == 0 {
			delete(h.subs, taskID)
		}
		h.mu.Unlock()
		entry.tryClose()
	}()

	return entry.ch
}

// Broadcast は taskID の全 subscriber に ev を非ブロッキングで配送する。
// チャネルが満杯の subscriber はドロップする（hub 全体を止めない）。
func (h *TaskEventHub) Broadcast(taskID string, ev TaskEvent) {
	h.mu.Lock()
	entries := make([]*subEntry, 0, len(h.subs[taskID]))
	for _, e := range h.subs[taskID] {
		entries = append(entries, e)
	}
	h.mu.Unlock()

	for _, e := range entries {
		e.trySend(ev)
	}
}

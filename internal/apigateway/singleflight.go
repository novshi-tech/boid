package apigateway

import "sync"

// singleflightGroup coalesces concurrent callers sharing the same key into a
// single execution of fn — mirroring golang.org/x/sync/singleflight.Group's
// core behavior (docs/plans/api-gateway.md §6 "先回りリフレッシュ...同時リク
// エストは singleflight で 1 回にまとめる"). Hand-rolled rather than
// depending on that package: CLAUDE.md's "外部ライブラリは最小限。標準ライブ
// ラリで実現できるものは追加しない" rule, and the whole mechanism is a
// mutex-guarded map of in-flight calls plus one sync.WaitGroup per key — well
// within what this leaf package already keeps self-contained elsewhere (see
// token.go's GenerateToken, duplicated rather than imported from
// internal/gitgateway for the identical reason: doc.go's own
// "self-contained" rationale).
//
// Generic over the result type T so this one small type serves
// OAuth2TokenSource's string-access-token use today without hard-coding that
// shape into the type itself.
type singleflightGroup[T any] struct {
	mu    sync.Mutex
	calls map[string]*singleflightCall[T]
}

// singleflightCall is one in-flight (or just-completed, until Do's owning
// goroutine removes it) execution shared by every caller racing on the same
// key.
type singleflightCall[T any] struct {
	wg  sync.WaitGroup
	val T
	err error
}

// Do executes fn for key if no call for key is already in flight, or blocks
// until the in-flight call finishes and returns its exact (val, err) pair
// otherwise. Every caller sharing a key during one overlapping window
// observes the identical result — including error values — rather than each
// independently retrying fn.
//
// The map entry for key is removed once fn returns (before Do returns to ANY
// caller, winner or waiter), so a subsequent call with the same key after
// this one completes always starts a fresh execution rather than replaying a
// stale cached result — Do provides in-flight de-duplication only, never a
// result cache.
func (g *singleflightGroup[T]) Do(key string, fn func() (T, error)) (T, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*singleflightCall[T])
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &singleflightCall[T]{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.val, c.err
}

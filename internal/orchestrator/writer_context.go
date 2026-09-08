package orchestrator

// The sandbox-write origin facts CreateAction's card-event ingest step needs
// and Action.Actor cannot supply: actor is stamped ActorHuman even for
// sandbox-originated calls, so it is not trustworthy evidence of who wrote
// an action.

import "context"

type writerCardRequestContextKey struct{}

// WithWriterCardRequestID marks ctx as carrying a sandbox write's origin
// card_requests row id, stamped by ExecuteBoidBuiltin
// (internal/server/boid_executor.go) — the one call site every
// sandbox-originated write funnels through. id is "" for a write that is not
// a card-command launcher/continuation.
func WithWriterCardRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, writerCardRequestContextKey{}, id)
}

// WriterCardRequestIDFromContext returns the card_requests row id
// WithWriterCardRequestID stamped, and whether ctx carries one at all.
func WriterCardRequestIDFromContext(ctx context.Context) (id string, ok bool) {
	v := ctx.Value(writerCardRequestContextKey{})
	if v == nil {
		return "", false
	}
	id, ok = v.(string)
	return id, ok
}

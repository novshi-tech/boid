package sandbox

import "context"

// BoidExecutor performs typed boid builtin operations after broker validation.
//
// ctx is the broker-side request context. For most ops it carries no deadline,
// but for the blocking BoidOpTaskAsk it is cancelled when the broker shuts down
// or the sandbox connection closes, so the server-side handler can stop waiting
// for an answer and clean up.
type BoidExecutor interface {
	ExecuteBoidBuiltin(ctx context.Context, tokenCtx TokenContext, req *BoidRequest) *ExecResponse
}

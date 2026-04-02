package sandbox

// BoidExecutor performs typed boid builtin operations after broker validation.
type BoidExecutor interface {
	ExecuteBoidBuiltin(ctx TokenContext, req *BoidRequest) *ExecResponse
}

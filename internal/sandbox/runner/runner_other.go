//go:build !linux

package runner

import "errors"

// errUnsupported is returned by the runner entry points on non-Linux platforms.
// boid only supports Linux; these stubs exist so the command layer compiles
// everywhere while the syscall implementation stays in runner_linux.go.
var errUnsupported = errors.New("sandbox runner is only supported on Linux")

func RunOuter(specPath, statePath string) (int, error)      { return 1, errUnsupported }
func RunInner(specPath, statePath string) (int, error)      { return 1, errUnsupported }
func RunInnerChild(specPath, statePath string) (int, error) { return 1, errUnsupported }
func RunContainer(specPath, statePath string) (int, error)  { return 1, errUnsupported }

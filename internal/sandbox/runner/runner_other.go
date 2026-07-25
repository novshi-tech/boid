//go:build !linux

package runner

import "errors"

// errUnsupported is returned by the runner entry points on non-Linux platforms.
// boid only supports Linux; this stub exists so the command layer compiles
// everywhere while the syscall implementation stays in runner_container_linux.go.
var errUnsupported = errors.New("sandbox runner is only supported on Linux")

func RunContainer(specPath, statePath string) (int, error) { return 1, errUnsupported }

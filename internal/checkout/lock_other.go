//go:build !linux

package checkout

import (
	"context"
	"fmt"
)

func lockCache(_ context.Context, _ string) (func(), error) {
	return nil, fmt.Errorf("git cache locking is unsupported on this platform")
}

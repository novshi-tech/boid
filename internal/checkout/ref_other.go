//go:build !linux

package checkout

import "os"

func openCacheRefFile(path string) (*os.File, error) {
	// Shared cache locking (and hence cache refresh) is unsupported here.
	return os.Open(path)
}

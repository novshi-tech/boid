package web_test

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoVendorDirInStatic guards against placing any directory named "vendor"
// under web/static/. Go's module zip generator (golang.org/x/mod/zip,
// isVendoredPackage) strips files whose path contains "/vendor/" followed by
// a subdirectory, so a web/static/vendor/foo/ subtree would be omitted from
// the module zip and cause 404s when boid is installed via "go install".
// Files directly under a "vendor" directory (e.g. vendor/foo.js) are also
// omitted because isVendoredPackage checks for the pattern.
func TestNoVendorDirInStatic(t *testing.T) {
	err := filepath.WalkDir("static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, component := range strings.Split(filepath.ToSlash(path), "/") {
			if component == "vendor" {
				t.Errorf("web/static contains a path component named %q: %s — "+
					"rename it (e.g. to 'assets') to avoid go module zip omission "+
					"(golang.org/x/mod/zip isVendoredPackage)", component, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}

package web_test

import (
	"io/fs"
	"os"
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

// TestTerminalResizeSetsMaxHeight guards a non-obvious mobile fix: the soft
// keyboard must not hide the special keybar at the bottom of the terminal.
//
// .boid-terminal is a `flex: 1 1 0` item inside the explicit-height flex
// column .site-main. A flex item with flex-basis:0 + flex-grow:1 IGNORES its
// `height` (flex-grow stretches it to fill the column), so resizeToViewport's
// inline `height` was a no-op on the job-detail page: when the soft keyboard
// shrank visualViewport, the terminal — and the keybar at its bottom — stayed
// at full height, hidden behind the keyboard. `max-height` DOES clamp
// flex-grow, so resizeToViewport must set it. If someone "cleans up" the
// seemingly-redundant max-height, the keybar regresses on mobile.
func TestTerminalResizeSetsMaxHeight(t *testing.T) {
	src, err := os.ReadFile("static/boid-terminal.js")
	if err != nil {
		t.Fatalf("read boid-terminal.js: %v", err)
	}
	if !strings.Contains(string(src), "style.maxHeight") {
		t.Error("boid-terminal.js resizeToViewport must set rootEl.style.maxHeight — " +
			"plain `height` is ignored by flex-grow:1 (flex-basis:0), so the special " +
			"keybar would be hidden behind the soft keyboard on mobile")
	}
}

// TestTerminalResizeHasNoFixedFloor guards a recurring mobile regression: the
// special keybar at the bottom of the terminal must never be clipped when the
// visible area is short (small phones, landscape, tall header, or an open soft
// keyboard shrinking visualViewport).
//
// resizeToViewport sizes .boid-terminal to fit between its top and the bottom of
// the visible area. An earlier version clamped that to `Math.max(200, …)`. When
// the available space dropped below the floor, .boid-terminal grew taller than
// the viewport and .site-main's `overflow:hidden` clipped its bottom — the
// keybar — out of view. The floor must stay at 0: the xterm viewport
// (flex:1 1 0, min-height:0) absorbs the shrink so the keybar (flex-shrink:0)
// stays visible. If someone reintroduces a non-zero floor, the keybar regresses.
func TestTerminalResizeHasNoFixedFloor(t *testing.T) {
	src, err := os.ReadFile("static/boid-terminal.js")
	if err != nil {
		t.Fatalf("read boid-terminal.js: %v", err)
	}
	if strings.Contains(string(src), "Math.max(200") {
		t.Error("boid-terminal.js resizeToViewport must not clamp the terminal " +
			"height to a fixed floor (e.g. Math.max(200, …)) — when the visible " +
			"area is shorter than the floor, .boid-terminal overflows and " +
			".site-main's overflow:hidden clips the special keybar. Clamp at 0 only")
	}
}

package packconformance

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// discoverPackDirs finds every Pack VERSION directory under root by
// walking for "integration.yaml" files — the discovery half of the CI
// sweep (TestOfficialPacks below). Deliberately NOT
// integrationpack.LoadPacks(root): LoadPacks treats every entry of its
// root as a Pack directory, which breaks on a Pack REPO checkout root
// because of that checkout's own .git/ directory (docs/plans/signal-
// ingest-detailed-design.md §12.1, "B1" — reproduced, not yet fixed as of
// this package; ConformancePack's own doc comment repeats this same
// reasoning). Walking for the marker file avoids the bug entirely rather
// than working around it, and is layout-agnostic besides — it works
// whether Packs end up living at the repo root or under some packs/
// subdirectory.
//
// Any directory whose name starts with "." (root itself excepted) is
// skipped outright, .git chief among them — nothing under a dot-directory
// is ever a Pack directory, and .git in particular is large enough that
// walking into it for nothing would be wasteful. This is exactly the class
// of bug PR-8's own predecessor (B1) shipped with no regression test at
// all; see TestDiscoverPackDirs below, which exercises this against a
// synthetic .git/ + a nested dot-directory + one real Pack, all in one
// t.TempDir() fixture, specifically so this can never silently regress the
// same way again.
//
// Returned paths are sorted for deterministic t.Run subtest ordering.
func discoverPackDirs(root string) ([]string, error) {
	var packDirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "integration.yaml" {
			packDirs = append(packDirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(packDirs)
	return packDirs, nil
}

// TestDiscoverPackDirs is F5's regression test: a synthetic checkout with
// a top-level .git/ directory (containing its own, bogus
// "integration.yaml" — proving it really is skipped, not just absent), a
// nested dot-directory elsewhere in the tree, and exactly one real Pack
// directory. Before this test existed, discoverPackDirs' dot-directory
// skip (the exact fix for the B1 class of bug — LoadPacks misreading a
// checkout's own .git/ as a Pack directory) was exercised only by manual,
// un-automated verification during this package's own development —
// precisely the kind of regression B1 itself was.
func TestDiscoverPackDirs(t *testing.T) {
	root := t.TempDir()

	// A .git/ directory with its own integration.yaml — must be skipped
	// entirely, not just "not counted" (if the walk descended into it at
	// all, this file would otherwise be discovered).
	mustMkdirAll(t, filepath.Join(root, ".git", "1.0.0"))
	mustWriteFile(t, filepath.Join(root, ".git", "1.0.0", "integration.yaml"), "bogus: true\n")

	// A nested dot-directory elsewhere in the tree (not just at root) —
	// the skip must apply at any depth, not only immediately under root.
	mustMkdirAll(t, filepath.Join(root, "some-pack", ".hidden-version"))
	mustWriteFile(t, filepath.Join(root, "some-pack", ".hidden-version", "integration.yaml"), "bogus: true\n")

	// One real Pack directory that must be found.
	mustMkdirAll(t, filepath.Join(root, "real-pack", "1.0.0"))
	mustWriteFile(t, filepath.Join(root, "real-pack", "1.0.0", "integration.yaml"), "real: true\n")

	got, err := discoverPackDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "real-pack", "1.0.0")}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("discoverPackDirs(%q) = %v, want %v", root, got, want)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOfficialPacks is the CI-facing half of Q22 (docs/plans/signal-driven-
// review.md §14: "Pack contract の conformance test が boid 側に存在し、公式
// Pack 全てがそれを通る"). It is gated on the BOID_API_SKILLS_DIR env var
// rather than running unconditionally under `go test ./...`, because the
// Pack content it exercises lives in a SEPARATE repo (boid-api-skills —
// docs/plans/signal-ingest-detailed-design.md §10's PR-8 note) this repo's
// own sandbox never has checked out. Run it locally against any checkout:
//
//	BOID_API_SKILLS_DIR=/path/to/boid-api-skills \
//	  go test ./packconformance/... -run TestOfficialPacks -v
//
// .github/workflows/blackbox-e2e.yml's pack-conformance job sets that env
// var to a sibling actions/checkout of novshi-tech/boid-api-skills and
// runs exactly this.
func TestOfficialPacks(t *testing.T) {
	root := os.Getenv("BOID_API_SKILLS_DIR")
	if root == "" {
		t.Skip("BOID_API_SKILLS_DIR not set — set it to a boid-api-skills checkout to run this sweep locally (see this test's own doc comment)")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("BOID_API_SKILLS_DIR=%q is not a directory: %v", root, err)
	}

	packDirs, err := discoverPackDirs(root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(packDirs) == 0 {
		// Not a failure: as of this package, boid-api-skills has not yet
		// been restructured into Pack layout (docs/plans/signal-ingest-
		// detailed-design.md §10's PR-8 is the follow-up that adds
		// integration.yaml + connectors/ to it). Failing this job before
		// that lands would make it permanently red for a reason unrelated
		// to this package. Once the first Pack lands, this branch stops
		// being reached and every discovered Pack actually runs through
		// ConformancePack below.
		t.Log("no integration.yaml found under BOID_API_SKILLS_DIR yet — nothing to check (expected until PR-8 lands the first official Pack)")
		return
	}

	for _, dir := range packDirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			rel = dir
		}
		t.Run(rel, func(t *testing.T) {
			ConformancePack(t, dir)
		})
	}
}

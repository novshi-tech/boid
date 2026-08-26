package conformance

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOfficialPacks is the CI-facing half of Q22 (docs/plans/signal-driven-
// review.md §14: "Pack contract の conformance test が boid 側に存在し、公式
// Pack 全てがそれを通る"). It is gated on the BOID_API_SKILLS_DIR env var
// rather than running unconditionally under `go test ./...`, because the
// Pack content it exercises lives in a SEPARATE repo (boid-api-skills —
// docs/plans/signal-ingest-detailed-design.md §10's PR-8 note) this repo's
// own sandbox never has checked out. Run it locally against any checkout:
//
//	BOID_API_SKILLS_DIR=/path/to/boid-api-skills \
//	  go test ./internal/integrationpack/conformance/... -run TestOfficialPacks -v
//
// .github/workflows/blackbox-e2e.yml's pack-conformance job sets that env
// var to a sibling actions/checkout of novshi-tech/boid-api-skills and
// runs exactly this.
//
// Pack directories are discovered by WALKING for integration.yaml files
// rather than calling integrationpack.LoadPacks on BOID_API_SKILLS_DIR
// directly — LoadPacks treats every entry of its root as a Pack directory,
// which breaks on a Pack REPO checkout root because of that checkout's own
// .git/ directory (docs/plans/signal-ingest-detailed-design.md §12.1,
// "B1" — reproduced, not yet fixed as of this package; ConformancePack's
// own doc comment repeats this same reasoning). Walking for the marker
// file avoids the bug entirely rather than working around it, and is
// layout-agnostic besides — it works whether Packs end up living at the
// repo root or under some packs/ subdirectory.
func TestOfficialPacks(t *testing.T) {
	root := os.Getenv("BOID_API_SKILLS_DIR")
	if root == "" {
		t.Skip("BOID_API_SKILLS_DIR not set — set it to a boid-api-skills checkout to run this sweep locally (see this test's own doc comment)")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("BOID_API_SKILLS_DIR=%q is not a directory: %v", root, err)
	}

	var packDirs []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// §12.1 (B1): skip dot-directories (.git chief among them) —
			// nothing under them is ever a Pack directory, and .git in
			// particular is large enough that walking into it for nothing
			// would be wasteful.
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
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
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

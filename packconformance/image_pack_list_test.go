package packconformance

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestImageShipsEveryOfficialPack ties the Dockerfile's hand-written list of
// Pack directories to what the Pack repo actually contains.
//
// Every other check in this package answers "is this Pack well formed". This
// one answers a question none of them can: **does the running daemon have it
// at all.** The image copies Packs by name —
//
//	cp -r /tmp/boid-api-skills/jira-cloud /tmp/boid-api-skills/slack ... /opt/boid/integrations/
//
// — so a Pack added to the repo and forgotten here passes conformance in CI
// (which walks the whole checkout) and then simply does not exist at runtime.
// The failure that produces is the worst-shaped one available: a workspace
// declares `signals.sources[].connector: <pack>/<connector>`, the daemon
// answers "pack not found" from a single derived trigger, and every OTHER
// source keeps working — so the inbox is not empty, the sweep still runs, and
// nothing about the system looks broken except that one source is silently
// never read.
//
// Skipped unless BOID_API_SKILLS_DIR is set, exactly like TestOfficialPacks:
// both halves have to be present, and only the pack-conformance CI job (and a
// developer who checks the repo out deliberately) has them.
func TestImageShipsEveryOfficialPack(t *testing.T) {
	skillsDir := os.Getenv("BOID_API_SKILLS_DIR")
	if skillsDir == "" {
		t.Skip("BOID_API_SKILLS_DIR is not set; skipping (see TestOfficialPacks)")
	}

	packDirs, err := discoverPackDirs(skillsDir)
	if err != nil {
		t.Fatalf("discover Packs under %s: %v", skillsDir, err)
	}
	inRepo := map[string]bool{}
	for _, dir := range packDirs {
		// discoverPackDirs returns the VERSION directory; the Pack name is
		// its parent, which is what the Dockerfile copies.
		inRepo[filepath.Base(filepath.Dir(dir))] = true
	}
	if len(inRepo) == 0 {
		t.Fatalf("no Packs found under %s — with nothing to compare, an empty diff would mean nothing", skillsDir)
	}

	dockerfile := filepath.Join("..", "build", "container", "Dockerfile")
	raw, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", dockerfile, err)
	}
	copied := packsCopiedByDockerfile(string(raw))
	if len(copied) == 0 {
		t.Fatalf("could not find the Pack copy line in %s — if that step changed shape, update this test rather than deleting it: an unparsed list compares as empty and every Pack would look missing", dockerfile)
	}

	var missing []string
	for name := range inRepo {
		if !copied[name] {
			missing = append(missing, name)
		}
	}
	var stale []string
	for name := range copied {
		if !inRepo[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("these Packs exist in the Pack repo but the image never copies them: %s\n"+
			"Their connectors resolve to \"pack not found\" at dispatch, which shows up as one source silently never being read. Add them to the `cp -r` in %s",
			strings.Join(missing, ", "), dockerfile)
	}
	if len(stale) > 0 {
		t.Errorf("the image copies these, but the Pack repo has no such Pack: %s\n"+
			"The `cp -r` will fail and the image build with it. Remove them from %s (or restore the Pack)",
			strings.Join(stale, ", "), dockerfile)
	}
}

// packsCopiedByDockerfile pulls the Pack names out of the Dockerfile's
// `cp -r /tmp/boid-api-skills/<name> ... /opt/boid/integrations/` step.
//
// A regex over the source rather than anything cleverer: the step is one
// literal line by construction (it has to be, to stay a single RUN layer),
// and the alternative — teaching the test to run docker — would make it
// unrunnable in the very job that needs it.
func packsCopiedByDockerfile(dockerfile string) map[string]bool {
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`/tmp/boid-api-skills/([a-z0-9][a-z0-9-]*)`).FindAllStringSubmatch(dockerfile, -1) {
		names[m[1]] = true
	}
	return names
}

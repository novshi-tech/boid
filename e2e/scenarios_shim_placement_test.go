package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestScenariosNoStaleShimPlacementReferences guards e2e/scenarios/ against
// re-introducing a dependency on the pre-5a-3 shim placement contract that
// Phase 5 (subtrack 5a) retired. See
// docs/plans/phase5-shim-and-task-context.md "PR 分割案 > 5a" — the cutover
// itself is 5a-3 (PR #806) and this static guard is added in the 5a-4
// completion pass.
//
// The retired shapes (any of these appearing in a live scenario is a
// regression):
//
//   - `BOID_HOST_COMMAND_NAMES`: env-map side channel that used to bridge the
//     aliased-file-basename case (`host_commands.<name>.path` pointing at a
//     file whose basename differed from the declared name). After 5a-3, the
//     shim is a symlink at `<sandboxShimBinDir>/<declared name>` so the
//     argv[0] basename is authoritative and no env-map is consulted.
//   - `hostCommandMounts` / `ResolveShimCommandName` / `shimBinaryPath`:
//     retired symbols (deleted from `internal/dispatcher/sandbox_builder.go`
//     and `internal/sandbox/shim.go` in the 5a-3 cutover). Scenarios should
//     never grep for them or reference them as live behavior.
//   - `/opt/boid/bin`: the shim directory the plan doc originally sketched.
//     5a-3 codex review moved the actual path to `/run/boid/bin` (`/opt` is
//     in the base rbind list — a mount there either EACCESs on typical
//     Linux hosts or leaks a symlink onto the host filesystem). Any live
//     scenario mentioning `/opt/boid/bin` is either stale or about to break
//     under real dispatch.
//
// Exemptions (same shape as TestScenariosNoStaleTaskContextFilePaths):
//
//   - Scenarios carrying a `skip` marker file are excluded wholesale
//     (`e2e/run.sh` doesn't run them by default; their content is
//     documentation of retired/blocked behavior).
//   - Lines whose trimmed content starts with `#` (comment lines) are
//     excluded. Scenarios may legitimately mention a retired shape in prose
//     while explaining the 5a-3 cutover in a hook/fixture comment (e.g.
//     `e2e/fixtures/hostbin/echo-target`) — those aren't live dependencies.
func TestScenariosNoStaleShimPlacementReferences(t *testing.T) {
	const scenariosRoot = "scenarios"

	entries, err := os.ReadDir(scenariosRoot)
	if err != nil {
		t.Fatalf("read %s: %v", scenariosRoot, err)
	}

	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\bBOID_HOST_COMMAND_NAMES\b`),
		regexp.MustCompile(`\bhostCommandMounts\b`),
		regexp.MustCompile(`\bResolveShimCommandName\b`),
		regexp.MustCompile(`\bshimBinaryPath\b`),
		regexp.MustCompile(`/opt/boid/bin`),
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scenarioDir := filepath.Join(scenariosRoot, entry.Name())
		if _, statErr := os.Stat(filepath.Join(scenarioDir, "skip")); statErr == nil {
			continue // skip-marked: see the scenario's own `skip` file for rationale
		}

		walkErr := filepath.WalkDir(scenarioDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				for _, re := range patterns {
					if re.MatchString(line) {
						t.Errorf("%s:%d references a retired shim-placement shape (matched %s); post-5a-3, the shim is a symlink at `<sandboxShimBinDir>/<declared name>` (see internal/dispatcher/sandbox_builder.go) and argv[0] basename is authoritative:\n\t%s",
							path, i+1, re.String(), strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", scenarioDir, walkErr)
		}
	}
}

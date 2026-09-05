package packconformance

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/integrationpack"
)

// DefaultLaunchTimeout bounds how long the connector-launch smoke check
// (checkConnectorLaunchable) waits for a connector process to exit before
// killing it and failing that connector's subtest. Generous relative to
// what it's actually exercising — a connector reacting to an immediately-
// refused BOID_API_BASE connection (§5.3) — because a Pack author's local
// `go test` run may take longer to start an interpreter than CI does.
const DefaultLaunchTimeout = 10 * time.Second

// Options configures ConformancePackOpts. The zero Options is what
// ConformancePack uses.
type Options struct {
	// LaunchTimeout overrides DefaultLaunchTimeout for the connector-launch
	// smoke check. Zero (the default) means DefaultLaunchTimeout.
	LaunchTimeout time.Duration

	// SkipLaunch disables the connector-launch smoke check entirely. The
	// existence/executable-bit check always runs regardless of this flag —
	// only the "actually run it and confirm it doesn't immediately crash or
	// hang" best-effort half is optional.
	//
	// Use this when the environment running the conformance test cannot
	// provide a connector's own runtime dependency (e.g. no python3 on a
	// Pack author's machine, or a stripped-down CI image), which is not
	// necessarily the same image the connector actually runs in.
	SkipLaunch bool
}

// ConformancePack runs every machine-checkable Pack contract requirement
// against the Pack VERSION directory at dir — a directory that itself
// contains integration.yaml, i.e. one <pack-name>/<version>/ directory such
// as integrationpack.Pack.Dir, NOT the multi-pack installation root
// integrationpack.LoadPacks walks (<integrations.dir>/<pack>/<version>/).
//
// This deliberately does NOT call LoadPacks: LoadPacks treats every entry
// of its root directory as a Pack directory, which breaks on a Pack REPO
// checkout root specifically because of that checkout's own .git/
// directory — LoadPacks goes looking for
// "<root>/.git/<version>/integration.yaml", finds nothing, and returns a
// hard error. ConformancePack sidesteps that failure mode entirely by
// taking a single, already-known-good Pack directory rather than a root to
// enumerate. officialpacks_test.go's own discovery (walking for
// integration.yaml files, explicitly skipping dot-directories) avoids the
// same bug the same way — see its own doc comment.
//
// Every requirement runs as its own t.Run subtest so one failing check
// does not hide the rest.
func ConformancePack(t *testing.T, dir string) {
	t.Helper()
	ConformancePackOpts(t, dir, Options{})
}

// ConformancePackOpts is ConformancePack with explicit Options — see
// Options' own doc comment for what each field relaxes and why a Pack
// author might need to.
func ConformancePackOpts(t *testing.T, dir string, opts Options) {
	t.Helper()

	var manifest *integrationpack.Manifest
	t.Run("manifest", func(t *testing.T) {
		t.Helper()
		m, err := parseManifestFile(dir)
		if err != nil {
			t.Fatal(err)
		}
		manifest = m
	})
	if manifest == nil {
		// The "manifest" subtest above already reported why. Every check
		// below needs a parsed manifest to know what to look at, so there
		// is nothing left to run.
		return
	}

	t.Run("skill_no_boid_command_references", func(t *testing.T) {
		checkSkillsNoBoidCommands(t, dir, manifest)
	})

	t.Run("connector_executable", func(t *testing.T) {
		checkConnectorsExecutable(t, dir, manifest)
	})

	if !opts.SkipLaunch {
		launchTimeout := opts.LaunchTimeout
		if launchTimeout <= 0 {
			launchTimeout = DefaultLaunchTimeout
		}
		for _, c := range manifest.Connectors {
			t.Run("connector_launchable/"+c.Name, func(t *testing.T) {
				checkConnectorLaunchable(t, dir, c, launchTimeout)
			})
		}
	}

	t.Run("no_go_source_or_internal_import", func(t *testing.T) {
		checkNoExtensionEscape(t, dir)
	})
}

// parseManifestFile reads and parses <dir>/integration.yaml via
// integrationpack.ParseManifest; this package has no manifest-parsing logic
// of its own.
func parseManifestFile(dir string) (*integrationpack.Manifest, error) {
	path := filepath.Join(dir, "integration.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("conformance: read %s: %w", path, err)
	}
	m, err := integrationpack.ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("conformance: %s: %w", path, err)
	}
	return m, nil
}

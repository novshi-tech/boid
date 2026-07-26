// Package homeenv isolates a test binary from the real developer machine's
// $HOME and XDG base directories, and lets a test assert that the isolation
// is actually in place.
//
// Why this exists as a package rather than a copy-pasted TestMain body: boid
// resolves several persistent roots — the workspace homes root
// (dispatcher.WorkspaceHomesDir), the workspace home init bookkeeping root
// (dispatcher's workspaceHomeMetaDir, docs/plans/workspace-home-volume-persistence.md
// 論点b), the daemon's own ~/.config/boid — from $XDG_DATA_HOME /
// $XDG_CONFIG_HOME / $HOME whenever the corresponding *Runner field is left
// empty, and a bare `dispatcher.Runner{}` is the standard minimal test
// wiring across several packages. Any package whose tests reach
// Runner.Dispatch (directly, or through a service that does) therefore has
// to install the same isolation, and every additional hand-rolled copy is
// another place for the set of variables to drift out of sync — the same
// single-source reasoning internal/dockerres was created for in PR1 of that
// same plan doc.
//
// Everything here is test-only. It is imported exclusively from _test.go
// files, so it never links into the shipped binary despite importing
// "testing".
package homeenv

import (
	"os"
	"path/filepath"
	"testing"
)

// isolatedKeys is the exact set of environment variables Isolate overrides
// and AssertIsolated checks. Keep the two in lockstep — the whole point of
// this package is that there is one list, not one per TestMain.
//
//   - HOME: os.UserHomeDir's source, the last-resort root behind both
//     dispatcher.workspaceDataHomeRoot and os.UserConfigDir.
//   - XDG_DATA_HOME: ~/.local/share/boid — homes/, homes-meta/, boid.db,
//     runtimes/ (which since PR3 of
//     docs/plans/workspace-home-volume-persistence.md also holds the
//     materialized embedded skills, under runtimes/skills).
//   - XDG_CONFIG_HOME: ~/.config/boid — workspaces/<slug>/init.sh (which a
//     dispatch will happily EXECUTE if a marker miss makes it eligible),
//     host_commands.yaml, config.yaml.
//   - XDG_STATE_HOME: ~/.local/state/boid — daemon.LogFilePath.
var isolatedKeys = []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME"}

// original records each isolatedKeys entry as it was when this package was
// initialized. Go initializes an imported package's variables before the
// importing test binary's TestMain runs, so this is the developer's (or CI
// runner's) real environment even when TestMain calls Isolate on the very
// first line — which is what makes AssertIsolated able to tell "isolated"
// from "not isolated" without being told.
var original = func() map[string]string {
	m := make(map[string]string, len(isolatedKeys))
	for _, key := range isolatedKeys {
		m[key] = os.Getenv(key)
	}
	return m
}()

// Run is the whole body of a TestMain that needs nothing else:
//
//	func TestMain(m *testing.M) { os.Exit(homeenv.Run(m)) }
//
// It points every isolatedKeys variable at a throwaway temp directory for the
// lifetime of the test binary, runs the suite, removes the temp directory and
// returns m.Run's exit code. Individual tests that want their own per-case
// isolation still call t.Setenv with a fresh t.TempDir(); that overrides this
// process-wide default for one test and is restored afterwards, so the two
// layers compose rather than conflict.
//
// A package whose TestMain has other work to do (spawning itself as a helper
// process, say) should call Isolate directly instead.
func Run(m *testing.M) int {
	cleanup, err := Isolate()
	if err != nil {
		panic(err)
	}
	defer cleanup()
	return m.Run()
}

// Isolate points HOME and the XDG base directories at a fresh temp directory
// and returns a function that removes it. Callers that use Isolate directly
// (rather than Run) are responsible for calling the returned cleanup — and
// for not calling os.Exit before it runs.
func Isolate() (cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "boid-test-home-")
	if err != nil {
		return nil, err
	}
	// Subdirectories rather than dir itself for the XDG roots, so a test that
	// inspects one of them cannot see the others' contents by accident.
	for key, val := range map[string]string{
		"HOME":            dir,
		"XDG_DATA_HOME":   filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
		"XDG_STATE_HOME":  filepath.Join(dir, "state"),
	} {
		if err := os.Setenv(key, val); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}

// AssertIsolated fails t when any isolatedKeys variable still holds the value
// it had when this test binary started — i.e. when the package under test has
// no TestMain calling Run/Isolate, or gained a new variable the list above
// covers but that TestMain does not.
//
// Call it from a test in every package whose suite can reach a code path that
// resolves a boid data/config root from the environment. Without it, losing
// the TestMain is silent: the suite keeps passing, it just starts writing
// into (and, via init.sh, executing code from) the real user's boid
// installation. That is not hypothetical — it is exactly what
// internal/server's suite did until PR2 of
// docs/plans/workspace-home-volume-persistence.md added its TestMain.
func AssertIsolated(t *testing.T) {
	t.Helper()
	for _, key := range isolatedKeys {
		got := os.Getenv(key)
		if got == original[key] {
			t.Errorf("%s = %q — unchanged since this test binary started, so this package's tests resolve boid's data/config roots from the REAL user environment. Add `func TestMain(m *testing.M) { os.Exit(homeenv.Run(m)) }` to this package.", key, got)
		}
	}
}

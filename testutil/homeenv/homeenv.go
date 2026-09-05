// Package homeenv isolates a test binary from the real developer machine's
// persistent boid state — $HOME, the XDG base directories, and the docker
// engine — and lets a test assert that the isolation is actually in place.
//
// This exists as a shared package rather than a copy-pasted TestMain body
// because boid resolves several persistent roots (workspace homes, the
// daemon's ~/.config/boid, and the docker engine holding workspace HOME
// volumes) from the environment whenever the corresponding *Runner field is
// left empty, and a bare `dispatcher.Runner{}` is the standard minimal test
// wiring across several packages — one shared list keeps every consumer's
// isolation in sync instead of drifting per-package.
//
// Everything here is test-only. It is imported from _test.go files and from
// testutil (itself test-only — it takes *testing.T), so it never links into
// the shipped binary despite importing "testing".
package homeenv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// isolatedKeys is the exact set of environment variables Isolate overrides
// and AssertIsolated checks. Keep the two in lockstep — the whole point of
// this package is that there is one list, not one per TestMain.
//
//   - HOME: os.UserHomeDir's source, the last-resort root behind both
//     dispatcher.workspaceDataHomeRoot and os.UserConfigDir.
//   - XDG_DATA_HOME: ~/.local/share/boid — homes/, homes-meta/, boid.db,
//     runtimes/ (including the materialized embedded skills).
//   - XDG_CONFIG_HOME: ~/.config/boid — workspaces/<slug>/init.sh (which a
//     dispatch will happily EXECUTE if a marker miss makes it eligible),
//     host_commands.yaml, config.yaml.
//   - XDG_STATE_HOME: ~/.local/state/boid — daemon.LogFilePath.
//   - DOCKER_HOST: the engine holding the workspace HOME volumes. See
//     dockerSocketName's comment.
//   - BOID_TEST_IMAGE_SKILLS_DIR: stands in for the runner IMAGE's
//     /opt/boid/skills. See imageSkillsEnv's comment.
var isolatedKeys = []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "DOCKER_HOST", imageSkillsEnv}

// clearedKeys are the remaining variables client.New(client.FromEnv) reads.
// They are UNSET by Isolate rather than redirected, so "isolated" means empty
// for them (Unisolated encodes that difference).
//
// They are not a safety hole on their own — DOCKER_HOST alone decides which
// engine is addressed — but leaving a developer's DOCKER_CERT_PATH /
// DOCKER_TLS_VERIFY / DOCKER_API_VERSION applied to the dead socket Isolate
// installs would make client.New load (or fail to load) their real TLS
// material, turning `daemon startup refused: connect to docker` into a
// machine-dependent test failure. The isolated environment should be
// self-consistent, not half-inherited.
var clearedKeys = []string{"DOCKER_API_VERSION", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// dockerSocketName is the basename Isolate points DOCKER_HOST at, inside its
// throwaway directory. Nothing ever creates it: an engine that cannot be
// dialled is the isolation.
//
// A workspace's $HOME is a docker named volume, and
// `DELETE /api/workspaces/{slug}` issues VolumeRemove(Force: true) against
// whatever engine DOCKER_HOST names — an unreachable engine has previously
// been the only thing standing between a test run and a real developer
// volume being destroyed. Redirecting the address is the only isolation
// that works for every consumer, since the engine handle is resolved deep
// inside buildRuntime rather than passed in.
const dockerSocketName = "docker-engine-must-not-be-reachable.sock"

// imageSkillsEnv names the variable that stands in for the runner IMAGE's
// /opt/boid/skills, where build/container/Dockerfile unpacks boid's embedded
// skills. internal/dispatcher's embeddedSkillsImageDir consults it.
//
// This is the odd one out on the list above: every other variable isolates a
// root the tests would otherwise WRITE to, while this one supplies a directory
// the test machine does not have at all. It joins the list for the same
// underlying reason, though — a boid data root resolved from the environment
// that must not be the developer's.
//
// It is needed because a workspace-home init now REFUSES to symlink a skill
// whose target directory is absent, deliberately: `ln -s` succeeds against a
// missing target, so without the check a runner image lacking /opt/boid/skills
// produces a home full of dangling links behind a completion marker that says
// the home is fine — silent, and permanent, since nothing afterwards re-runs.
// The consequence for tests is that any suite reaching a real Dispatch needs
// that path to exist, in whatever package it lives in, which is exactly the
// "one list, not one per TestMain" problem this package was created for.
//
// TEST is in the name because nothing in a deployment sets it: boid builds the
// image that carries the real path. A future container_image override that
// wants to relocate the directory should get a real config field rather than
// borrowing this.
const imageSkillsEnv = "BOID_TEST_IMAGE_SKILLS_DIR"

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

// Isolate points HOME, the XDG base directories and DOCKER_HOST at a fresh
// temp directory (clearing the remaining docker transport variables) and
// returns a function that removes it. Callers that use Isolate directly
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
		// A path inside dir, deliberately never created. client.New parses
		// the address and builds an HTTP transport for it without dialling,
		// so daemon startup still succeeds; every actual engine call then
		// fails with ENOENT, which is precisely the "no engine reachable"
		// degradation every workspace-home code path already handles.
		"DOCKER_HOST": "unix://" + filepath.Join(dir, dockerSocketName),
		// Unlike the entries above, this one is POPULATED below: the init
		// prelude checks each skill target with `[ -d ]`, so an empty
		// directory would fail every dispatch.
		imageSkillsEnv: filepath.Join(dir, "image-skills"),
	} {
		if err := os.Setenv(key, val); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	for _, key := range clearedKeys {
		if err := os.Unsetenv(key); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	// One empty directory per embedded skill, which is all the prelude's
	// `[ -d ]` check looks at. A test that needs the link to RESOLVE to real
	// content points its own SkillLink at its own fixture instead.
	for _, name := range skills.EmbeddedSkillNames() {
		if err := os.MkdirAll(filepath.Join(dir, "image-skills", name), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}

// Unisolated reports every variable this package manages that is still the
// developer's own: an isolatedKeys entry unchanged since the test binary
// started, or a clearedKeys entry that is still set. An empty result means
// Isolate (or Run) has taken effect.
//
// Exported as a plain predicate, rather than living inside AssertIsolated, so
// the same judgment can be made where there is no *testing.T-shaped assertion
// to hang off — specifically testutil.NewTestServer, which refuses to boot a
// daemon into an un-isolated environment. Two copies of this rule would be two
// places for the key list to drift.
func Unisolated() []string {
	var keys []string
	for _, key := range isolatedKeys {
		if os.Getenv(key) == original[key] {
			keys = append(keys, key)
		}
	}
	for _, key := range clearedKeys {
		if os.Getenv(key) != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// AssertIsolated fails t when Unisolated reports anything — i.e. when the
// package under test has no TestMain calling Run/Isolate, or gained a new
// variable the lists above cover but that TestMain does not.
//
// Call it from a test in every package whose suite can reach a code path that
// resolves a boid data/config root, or a docker engine, from the environment.
// Without it, losing the TestMain is silent: the suite keeps passing, it just
// starts writing into (and, via init.sh, executing code from) the real
// user's boid installation — and deleting named volumes off their real
// engine.
func AssertIsolated(t *testing.T) {
	t.Helper()
	for _, key := range Unisolated() {
		t.Errorf("%s = %q — still the value this test binary started with, so this package's tests resolve boid's data/config roots (and its docker engine) from the REAL user environment. Add `func TestMain(m *testing.M) { os.Exit(homeenv.Run(m)) }` to this package.", key, os.Getenv(key))
	}
}

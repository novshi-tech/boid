package integrationpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePack writes <dir>/<pack>/<version>/integration.yaml with the given
// manifest content, creating parent directories as needed.
func writePack(t *testing.T, dir, pack, version, manifestYAML string) {
	t.Helper()
	verDir := filepath.Join(dir, pack, version)
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, "integration.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jiraManifest(name, version string) string {
	return "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: " + name + ", version: '" + version + "'}\n" +
		"serviceProfiles:\n  - name: jira-cloud\n    endpoint: {configurable: true}\n    credentials:\n      - {name: token, injection: bearer}\n"
}

// TestLoadPacks_NonexistentDir pins LoadPacks' "not configured yet" case
// (docs/plans/signal-ingest-detailed-design.md §6.1's default
// integrations.dir, /opt/boid/integrations, will not exist on most
// deployments until an operator actually installs a Pack) — a missing root
// directory is an empty registry, not a startup error.
func TestLoadPacks_NonexistentDir(t *testing.T) {
	packs, err := LoadPacks(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("len(packs) = %d, want 0", len(packs))
	}
}

// TestLoadPacks_EmptyDir mirrors TestLoadPacks_NonexistentDir for a
// directory that exists but has nothing installed under it yet.
func TestLoadPacks_EmptyDir(t *testing.T) {
	packs, err := LoadPacks(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("len(packs) = %d, want 0", len(packs))
	}
}

// TestLoadPacks_Valid pins the enumeration contract: one *Pack per
// <dir>/<pack>/<version>/integration.yaml, Dir pointing at the version
// directory (the bind-mount source PR-5's derived trigger needs — signal-
// ingest-detailed-design.md §7.1 "mount 位置").
func TestLoadPacks_Valid(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "jira-cloud", "1.2.0", jiraManifest("jira-cloud", "1.2.0"))
	writePack(t, dir, "slack", "1.1.0", jiraManifest("slack", "1.1.0"))

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("len(packs) = %d, want 2", len(packs))
	}
	byName := make(map[string]*Pack, len(packs))
	for _, p := range packs {
		byName[p.Name] = p
	}
	jira, ok := byName["jira-cloud"]
	if !ok {
		t.Fatal("jira-cloud pack not loaded")
	}
	if jira.Version != "1.2.0" {
		t.Errorf("jira-cloud.Version = %q, want 1.2.0", jira.Version)
	}
	wantDir := filepath.Join(dir, "jira-cloud", "1.2.0")
	if jira.Dir != wantDir {
		t.Errorf("jira-cloud.Dir = %q, want %q", jira.Dir, wantDir)
	}
	if len(jira.Manifest.ServiceProfiles) != 1 {
		t.Errorf("jira-cloud.Manifest.ServiceProfiles = %+v", jira.Manifest.ServiceProfiles)
	}
	if _, ok := byName["slack"]; !ok {
		t.Fatal("slack pack not loaded")
	}
}

// TestLoadPacks_MultipleVersionsOfSamePack pins that two versions of the
// same Pack coexist as two distinct *Pack entries (a services.<name>.uses
// reference pins the exact version — docs/plans/signal-driven-review.md
// §7.2).
func TestLoadPacks_MultipleVersionsOfSamePack(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "jira-cloud", "1.2.0", jiraManifest("jira-cloud", "1.2.0"))
	writePack(t, dir, "jira-cloud", "1.3.0", jiraManifest("jira-cloud", "1.3.0"))

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("len(packs) = %d, want 2", len(packs))
	}
}

// TestLoadPacks_VersionDirMismatchRejected pins Q19's explicit v0
// restriction (docs/plans/signal-ingest-detailed-design.md §6.2: "<ver>
// ディレクトリ名が manifest の version と一致しない Pack は v0 では起動
// エラー") — no silent trust of whichever one is "right".
func TestLoadPacks_VersionDirMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	// Directory says 1.2.0, manifest says 1.3.0.
	writePack(t, dir, "jira-cloud", "1.2.0", jiraManifest("jira-cloud", "1.3.0"))

	_, err := LoadPacks(dir)
	if err == nil {
		t.Fatal("want error for version directory / manifest version mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "1.2.0") || !strings.Contains(err.Error(), "1.3.0") {
		t.Errorf("error should name both versions, got: %v", err)
	}
}

// TestLoadPacks_NameDirMismatchRejected is VersionDirMismatchRejected's
// pack-name counterpart: the manifest's own metadata.name must agree with
// the directory it was installed under, the same "don't silently trust
// whichever is right" posture.
func TestLoadPacks_NameDirMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "jira-cloud", "1.2.0", jiraManifest("wrong-name", "1.2.0"))

	_, err := LoadPacks(dir)
	if err == nil {
		t.Fatal("want error for pack directory / manifest name mismatch, got nil")
	}
}

// TestLoadPacks_MissingManifestRejected pins that a version directory with
// no integration.yaml at all is a hard error, not a silent skip — the
// curated integrations.dir layout (signal-driven-review.md §6.4) has no
// legitimate reason for a version directory without one.
func TestLoadPacks_MissingManifestRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "jira-cloud", "1.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPacks(dir)
	if err == nil {
		t.Fatal("want error for a version directory with no integration.yaml, got nil")
	}
}

// TestLoadPacks_InvalidManifestNamesTheFile pins that a manifest parse/
// validation failure names the offending integration.yaml path, so an
// operator with several installed Packs knows which one is broken.
func TestLoadPacks_InvalidManifestNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "jira-cloud", "1.2.0", "apiVersion: boid.dev/v2\nkind: IntegrationPack\nmetadata: {name: jira-cloud, version: '1.2.0'}\n")

	_, err := LoadPacks(dir)
	if err == nil {
		t.Fatal("want error for an invalid manifest, got nil")
	}
	if !strings.Contains(err.Error(), "integration.yaml") {
		t.Errorf("error should name integration.yaml, got: %v", err)
	}
}

// TestPack_ServiceProfile pins the lookup helper DesugarService (resolve.go)
// uses to find the profile a uses: reference names.
func TestPack_ServiceProfile(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "jira-cloud", "1.2.0", jiraManifest("jira-cloud", "1.2.0"))
	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := packs[0]
	sp, ok := p.ServiceProfile("jira-cloud")
	if !ok || sp.Name != "jira-cloud" {
		t.Errorf("ServiceProfile(jira-cloud) = (%+v, %v)", sp, ok)
	}
	if _, ok := p.ServiceProfile("nonexistent"); ok {
		t.Error("ServiceProfile(nonexistent) should not be found")
	}
}

package orchestrator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTestKit writes a minimal kit.yaml under <kitsDir>/<name>/kit.yaml.
func writeTestKit(t *testing.T, kitsDir, name, content string) {
	t.Helper()
	dir := filepath.Join(kitsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir %q: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func TestLoadHostCommandsFromKits_EmptyKitsDir(t *testing.T) {
	kitsDir := t.TempDir() // exists, but has no kit subdirectories

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLoadHostCommandsFromKits_MissingKitsDir(t *testing.T) {
	kitsDir := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLoadHostCommandsFromKits_SingleKitSingleCommand(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "gh-kit", `
host_commands:
  gh:
    allow: [pr, issue]
`)

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 command, got %v", got)
	}
	spec, ok := got["gh"]
	if !ok {
		t.Fatalf("expected 'gh' command, got %v", got)
	}
	if !reflect.DeepEqual(spec.Allow, []string{"pr", "issue"}) {
		t.Errorf("gh.Allow = %v, want [pr issue]", spec.Allow)
	}
}

func TestLoadHostCommandsFromKits_DisjointCommandsAcrossKits(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "gh-kit", `
host_commands:
  gh:
    allow: [pr]
`)
	writeTestKit(t, kitsDir, "aws-kit", `
host_commands:
  aws:
    allow: [s3]
`)

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 commands, got %v", got)
	}
	if _, ok := got["gh"]; !ok {
		t.Errorf("missing gh command: %v", got)
	}
	if _, ok := got["aws"]; !ok {
		t.Errorf("missing aws command: %v", got)
	}
}

func TestLoadHostCommandsFromKits_SameNameSameDefinitionDedupes(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "kit-a", `
host_commands:
  gh:
    allow: [pr, issue]
`)
	writeTestKit(t, kitsDir, "kit-b", `
host_commands:
  gh:
    allow: [pr, issue]
`)

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 command (deduped), got %v", got)
	}
}

func TestLoadHostCommandsFromKits_SameNameDifferentDefinitionFails(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "kit-a", `
host_commands:
  gh:
    allow: [pr]
`)
	writeTestKit(t, kitsDir, "kit-b", `
host_commands:
  gh:
    allow: [issue]
`)

	_, err := LoadHostCommandsFromKits(kitsDir)
	if err == nil {
		t.Fatal("expected error for conflicting host_command definitions")
	}
	msg := err.Error()
	if !strings.Contains(msg, filepath.Join(kitsDir, "kit-a")) {
		t.Errorf("error message should mention kit-a dir: %s", msg)
	}
	if !strings.Contains(msg, filepath.Join(kitsDir, "kit-b")) {
		t.Errorf("error message should mention kit-b dir: %s", msg)
	}
	if !strings.Contains(msg, "gh") {
		t.Errorf("error message should mention the conflicting command name: %s", msg)
	}
}

// TestLoadHostCommandsFromKits_MultipleCollisionsReportDeterministic pins the
// deterministic-error contract: when a single kit declares several commands
// that each collide with a prior kit, the *reported* collision must be the
// lexicographically-first colliding name, so identical input always surfaces
// the same error message across daemon starts. Without command-name sorting,
// Go's randomised map iteration would pick different collisions on different
// runs. Repeat the aggregation a few times to make a regression far more
// likely to trip if the ordering guarantee ever breaks.
func TestLoadHostCommandsFromKits_MultipleCollisionsReportDeterministic(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "kit-a", `
host_commands:
  aws:
    allow: [s3]
  gh:
    allow: [pr]
  zzz:
    allow: [do]
`)
	writeTestKit(t, kitsDir, "kit-b", `
host_commands:
  aws:
    allow: [ec2]
  gh:
    allow: [issue]
  zzz:
    allow: [redo]
`)

	for i := 0; i < 20; i++ {
		_, err := LoadHostCommandsFromKits(kitsDir)
		if err == nil {
			t.Fatalf("iteration %d: expected error for conflicting host_command definitions", i)
		}
		msg := err.Error()
		// "aws" is lexicographically first among the three colliding
		// commands, so it must be the one named in the error every time.
		if !strings.Contains(msg, `"aws"`) {
			t.Errorf("iteration %d: expected error to name lexicographically-first colliding command \"aws\", got: %s", i, msg)
		}
	}
}

func TestWriteHostCommandsConfig_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	spec := map[string]HostCommandSpec{
		"gh":  {Allow: []string{"pr", "issue"}},
		"aws": {Allow: []string{"s3"}, Path: "/usr/local/bin/aws"},
	}

	if err := WriteHostCommandsConfig(path, spec); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}

	got, err := LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Errorf("round trip mismatch:\ngot  %+v\nwant %+v", got, spec)
	}
}

func TestWriteHostCommandsConfig_FailurePreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host_commands.yaml")
	original := []byte("host_commands:\n  gh:\n    allow:\n      - pr\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	// Make the directory read-only so the temp-file create/rename fails,
	// simulating a write failure partway through.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := WriteHostCommandsConfig(path, map[string]HostCommandSpec{"aws": {}})
	if err == nil {
		t.Fatal("expected error writing to read-only dir")
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != string(original) {
		t.Errorf("existing file was modified on write failure:\ngot  %q\nwant %q", got, original)
	}
}

func TestDefaultHostCommandsPath_RespectsXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	want := filepath.Join(dir, "boid", "host_commands.yaml")
	if got != want {
		t.Errorf("DefaultHostCommandsPath: got %q, want %q", got, want)
	}
}

func TestLoadHostCommandsConfig_MissingFileReturnsEmptyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	got, err := LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLoadHostCommandsConfig_EmptyFileReturnsEmptyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLoadHostCommandsConfig_RejectsUnknownTopLevelField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	content := "host_commands:\n  gh:\n    allow: [pr]\nunknown_field: true\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadHostCommandsConfig(path)
	if err == nil {
		t.Fatal("expected error for unknown top-level field")
	}
}

// --- BLOCKER: env expansion must not happen in the aggregation path ---

// TestLoadHostCommandsFromKits_DoesNotExpandEnvPlaceholders pins the fix for
// the codex-review blocker: LoadHostCommandsFromKits must not run kit.yaml's
// host_commands through env-var interpolation (unlike ReadKitMeta, which
// expands ${VAR} for direct sandbox consumption). Expanding here would leak
// resolved secret values (e.g. a token) into the aggregated
// ~/.config/boid/host_commands.yaml on disk.
func TestLoadHostCommandsFromKits_DoesNotExpandEnvPlaceholders(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("GH_TOKEN", "super-secret-token")

	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "gh-kit", `
host_commands:
  gh:
    path: ${HOME}/bin/gh
    env:
      GH_TOKEN: ${GH_TOKEN}
`)

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: %v", err)
	}
	spec, ok := got["gh"]
	if !ok {
		t.Fatalf("expected 'gh' command, got %v", got)
	}
	if spec.Path != "${HOME}/bin/gh" {
		t.Errorf("Path should remain unexpanded, got %q", spec.Path)
	}
	if spec.Env["GH_TOKEN"] != "${GH_TOKEN}" {
		t.Errorf("Env[GH_TOKEN] should remain unexpanded, got %q", spec.Env["GH_TOKEN"])
	}
}

// TestLoadHostCommandsFromKits_DetectsCollisionWithDifferentPlaceholders pins
// that collision detection compares raw (unexpanded) values: two kits using
// differently-named placeholders that happen to resolve to the same daemon
// env value must still be treated as a definition conflict, since expansion
// (and thus "they're actually equal") never happens on this path.
func TestLoadHostCommandsFromKits_DetectsCollisionWithDifferentPlaceholders(t *testing.T) {
	t.Setenv("FOO", "/same/path")
	t.Setenv("BAR", "/same/path")

	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "kit-a", `
host_commands:
  gh:
    path: ${FOO}
`)
	writeTestKit(t, kitsDir, "kit-b", `
host_commands:
  gh:
    path: ${BAR}
`)

	_, err := LoadHostCommandsFromKits(kitsDir)
	if err == nil {
		t.Fatal("expected collision error even though $FOO == $BAR in the daemon env")
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("error should mention the conflicting command name: %v", err)
	}
}

// TestWriteHostCommandsConfig_UsesMode0600 pins that the aggregated config
// file is written with owner-only permissions, since it may carry unexpanded
// but still sensitive-looking placeholders (path/env values) sourced
// verbatim from kit.yaml.
func TestWriteHostCommandsConfig_UsesMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	spec := map[string]HostCommandSpec{"gh": {Allow: []string{"pr"}}}

	if err := WriteHostCommandsConfig(path, spec); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("host_commands.yaml perm: got %v, want 0600", perm)
	}
}

// --- MAJOR: nil vs empty slice/map normalization ---

// TestLoadHostCommandsFromKits_NilVsEmptyDedupes pins that two kits declaring
// an otherwise-identical command must dedupe even when one leaves a
// slice/map field nil (by omission) and the other spells it out empty
// (e.g. `env:` with nothing under it vs `env: {}`) — reflect.DeepEqual alone
// treats nil and empty as different, which would falsely report a
// definition conflict for what is semantically the same spec.
func TestLoadHostCommandsFromKits_NilVsEmptyDedupes(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "kit-a", `
host_commands:
  gh:
    allow: [pr]
    env:
`)
	writeTestKit(t, kitsDir, "kit-b", `
host_commands:
  gh:
    allow: [pr]
    env: {}
`)

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: expected dedupe, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 command (deduped), got %v", got)
	}
}

// TestLoadHostCommandsFromKits_NilVsEmptyRejectDedupes is the same case for
// the Reject []RejectRule field: one kit omits it (nil), the other spells it
// out as an empty list.
func TestLoadHostCommandsFromKits_NilVsEmptyRejectDedupes(t *testing.T) {
	kitsDir := t.TempDir()
	writeTestKit(t, kitsDir, "kit-a", `
host_commands:
  gh:
    allow: [pr]
`)
	writeTestKit(t, kitsDir, "kit-b", `
host_commands:
  gh:
    allow: [pr]
    reject: []
`)

	got, err := LoadHostCommandsFromKits(kitsDir)
	if err != nil {
		t.Fatalf("LoadHostCommandsFromKits: expected dedupe, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 command (deduped), got %v", got)
	}
}

// --- MAJOR: nested strict unmarshal for the aggregated config file ---

// TestLoadHostCommandsConfig_RejectsNestedUnknownField pins that a typo in a
// per-command field (e.g. "alow" instead of "allow") in the on-disk
// host_commands.yaml is rejected rather than silently discarded. The
// top-level dec.KnownFields(true) alone does not catch this: HostCommands'
// custom UnmarshalYAML runs a fresh, non-strict node.Decode internally, so
// the strict decode must instead target a plain map type that has no custom
// unmarshaler in its path.
func TestLoadHostCommandsConfig_RejectsNestedUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	content := "host_commands:\n  gh:\n    alow: [pr]\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadHostCommandsConfig(path)
	if err == nil {
		t.Fatal("expected error for nested unknown field (typo'd 'alow')")
	}
}

// TestLoadHostCommandsConfig_RejectsNestedUnknownFieldInReject pins the same
// strictness one level deeper, inside a reject rule entry.
func TestLoadHostCommandsConfig_RejectsNestedUnknownFieldInReject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	content := "host_commands:\n  gh:\n    reject:\n      - match: \"push --force*\"\n        reasonn: no force push\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadHostCommandsConfig(path)
	if err == nil {
		t.Fatal("expected error for nested unknown field in reject rule (typo'd 'reasonn')")
	}
}

// TestLoadHostCommandsConfig_ValidNestedFieldStillLoads is a regression guard
// alongside the two strictness tests above: an ordinary, fully-valid config
// (every documented HostCommandSpec field populated) must still load.
func TestLoadHostCommandsConfig_ValidNestedFieldStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	content := "" +
		"host_commands:\n" +
		"  gh:\n" +
		"    allow: [pr, issue]\n" +
		"    deny: [secret]\n" +
		"    path: /usr/local/bin/gh\n" +
		"    env:\n" +
		"      GH_TOKEN: xyz\n" +
		"    reject:\n" +
		"      - match: \"push --force*\"\n" +
		"        reason: no force push\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: unexpected error: %v", err)
	}
	spec, ok := got["gh"]
	if !ok {
		t.Fatalf("expected 'gh' command, got %v", got)
	}
	if !reflect.DeepEqual(spec.Allow, []string{"pr", "issue"}) {
		t.Errorf("Allow: got %v", spec.Allow)
	}
	if !reflect.DeepEqual(spec.Deny, []string{"secret"}) {
		t.Errorf("Deny: got %v", spec.Deny)
	}
	if spec.Path != "/usr/local/bin/gh" {
		t.Errorf("Path: got %q", spec.Path)
	}
	if spec.Env["GH_TOKEN"] != "xyz" {
		t.Errorf("Env[GH_TOKEN]: got %q", spec.Env["GH_TOKEN"])
	}
	if len(spec.Reject) != 1 || spec.Reject[0].Match != "push --force*" || spec.Reject[0].Reason != "no force push" {
		t.Errorf("Reject: got %+v", spec.Reject)
	}
}

// --- MINOR: HostCommands() / CloneHostCommandsMap deep-copy snapshot ---

// TestCloneHostCommandsMap_DeepCopiesSlicesAndMaps pins that
// CloneHostCommandsMap returns an independent copy: mutating the clone's
// top-level map, or any of a spec's slice/map fields, must never be visible
// through the original map. This is the unit-level guard for
// Server.HostCommands()'s "never returns internal state" contract (the
// integration-level guard lives in wire_host_commands_test.go).
func TestCloneHostCommandsMap_DeepCopiesSlicesAndMaps(t *testing.T) {
	original := map[string]HostCommandSpec{
		"gh": {
			Allow: []string{"pr"},
			Env:   map[string]string{"GH_TOKEN": "abc"},
			Reject: []RejectRule{
				{Match: "push --force*", Reason: "no force push"},
			},
		},
	}

	clone := CloneHostCommandsMap(original)

	// Mutate the clone's top-level map.
	clone["aws"] = HostCommandSpec{Allow: []string{"s3"}}
	if _, ok := original["aws"]; ok {
		t.Error("adding a key to the clone leaked into the original map")
	}

	// Mutate the clone's nested slice/map fields.
	ghClone := clone["gh"]
	ghClone.Allow[0] = "mutated"
	ghClone.Env["GH_TOKEN"] = "mutated"
	ghClone.Reject[0].Reason = "mutated"
	clone["gh"] = ghClone

	ghOriginal := original["gh"]
	if ghOriginal.Allow[0] != "pr" {
		t.Errorf("mutating clone's Allow leaked into original: %v", ghOriginal.Allow)
	}
	if ghOriginal.Env["GH_TOKEN"] != "abc" {
		t.Errorf("mutating clone's Env leaked into original: %v", ghOriginal.Env)
	}
	if ghOriginal.Reject[0].Reason != "no force push" {
		t.Errorf("mutating clone's Reject leaked into original: %v", ghOriginal.Reject)
	}
}

func TestCloneHostCommandsMap_NilInputReturnsNil(t *testing.T) {
	if got := CloneHostCommandsMap(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// --- MAJOR 2: ${VAR} expansion for dispatch ---

// TestExpandHostCommandsForDispatch_ExpandsPath pins MAJOR 2 (codex
// review): the aggregated host_commands.yaml config stores raw/unexpanded
// path/env values (PR2's "集約 config は raw で保存" decision), but dispatch
// needs them resolved against the daemon's own environment or
// exec.LookPath fails on the literal placeholder string.
// ExpandHostCommandsForDispatch is the bridge — called once at daemon
// startup on the raw config, its result (not the raw map) is what gets
// wired into ProjectStore.SetHostCommands.
func TestExpandHostCommandsForDispatch_ExpandsPath(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("GH_TOKEN", "super-secret-token")

	raw := map[string]HostCommandSpec{
		"gh": {
			Path: "${HOME}/bin/gh",
			Env:  map[string]string{"GH_TOKEN": "${GH_TOKEN}"},
		},
	}

	got := ExpandHostCommandsForDispatch(raw)
	gh, ok := got["gh"]
	if !ok {
		t.Fatalf("expected 'gh' command, got %v", got)
	}
	if gh.Path != "/home/testuser/bin/gh" {
		t.Errorf("Path = %q, want expanded /home/testuser/bin/gh", gh.Path)
	}
	if gh.Env["GH_TOKEN"] != "super-secret-token" {
		t.Errorf("Env[GH_TOKEN] = %q, want expanded super-secret-token", gh.Env["GH_TOKEN"])
	}
}

// TestExpandHostCommandsForDispatch_DoesNotMutateInput pins that raw is
// never mutated: the returned map (and every nested Env map) must be an
// independent copy, so the caller's original raw config (e.g. what
// Server.HostCommands() later hands out as a snapshot) stays unexpanded.
func TestExpandHostCommandsForDispatch_DoesNotMutateInput(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")

	raw := map[string]HostCommandSpec{
		"gh": {
			Path: "${HOME}/bin/gh",
			Env:  map[string]string{"FOO": "${HOME}"},
		},
	}

	_ = ExpandHostCommandsForDispatch(raw)

	if raw["gh"].Path != "${HOME}/bin/gh" {
		t.Errorf("input Path was mutated: got %q", raw["gh"].Path)
	}
	if raw["gh"].Env["FOO"] != "${HOME}" {
		t.Errorf("input Env was mutated: got %q", raw["gh"].Env["FOO"])
	}
}

// TestExpandHostCommandsForDispatch_NilInputReturnsNil is the same
// nil-safety contract CloneHostCommandsMap already guarantees, since
// ExpandHostCommandsForDispatch is built on top of it.
func TestExpandHostCommandsForDispatch_NilInputReturnsNil(t *testing.T) {
	if got := ExpandHostCommandsForDispatch(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// --- MAJOR 3 (codex review, 2nd pass): writeHostCommandsConfigIfMissing ---

// TestWriteHostCommandsConfigIfMissing_WritesWhenFileAbsent pins the "fresh
// install" half of the contract: with no file at path yet, the helper must
// write spec and report that it did.
func TestWriteHostCommandsConfigIfMissing_WritesWhenFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	spec := map[string]HostCommandSpec{"gh": {Allow: []string{"pr"}}}

	wrote, err := writeHostCommandsConfigIfMissing(path, spec)
	if err != nil {
		t.Fatalf("writeHostCommandsConfigIfMissing: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true when the file did not exist yet")
	}

	got, err := LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Errorf("written config = %+v, want %+v", got, spec)
	}
}

// TestWriteHostCommandsConfigIfMissing_SkipsWhenFileExists pins the "do not
// clobber" half: when a file already exists at path, the helper must leave
// it untouched and report that no write happened, regardless of what spec
// would have produced.
func TestWriteHostCommandsConfigIfMissing_SkipsWhenFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_commands.yaml")
	existing := map[string]HostCommandSpec{"custom-tool": {Allow: []string{"run"}}}
	if err := WriteHostCommandsConfig(path, existing); err != nil {
		t.Fatalf("seed existing config: %v", err)
	}

	wrote, err := writeHostCommandsConfigIfMissing(path, map[string]HostCommandSpec{"gh": {Allow: []string{"pr"}}})
	if err != nil {
		t.Fatalf("writeHostCommandsConfigIfMissing: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false when the file already existed")
	}

	got, err := LoadHostCommandsConfig(path)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if !reflect.DeepEqual(got, existing) {
		t.Errorf("existing config was modified: got %+v, want %+v", got, existing)
	}
}

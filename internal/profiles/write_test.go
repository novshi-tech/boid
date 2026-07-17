package profiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- SetProfile / RemoveProfile: pure in-memory mutation semantics ---

func TestSetProfile_AddsNewEntry_DoesNotMutateOriginal(t *testing.T) {
	orig := &Config{Profiles: map[string]Profile{"home": {URL: "unix:///tmp/a.sock"}}}
	out := SetProfile(orig, "work", Profile{URL: "https://work.example.com"})

	if len(orig.Profiles) != 1 {
		t.Fatalf("SetProfile must not mutate its input; orig.Profiles = %+v", orig.Profiles)
	}
	if len(out.Profiles) != 2 {
		t.Fatalf("expected 2 profiles in the result, got %d: %+v", len(out.Profiles), out.Profiles)
	}
	if got := out.Profiles["work"].URL; got != "https://work.example.com" {
		t.Errorf("work url = %q", got)
	}
	if got := out.Profiles["home"].URL; got != "unix:///tmp/a.sock" {
		t.Errorf("home url = %q", got)
	}
}

func TestSetProfile_UpdatesExistingEntry(t *testing.T) {
	orig := &Config{Profiles: map[string]Profile{"work": {URL: "https://old.example.com"}}}
	out := SetProfile(orig, "work", Profile{URL: "https://new.example.com"})

	if len(out.Profiles) != 1 {
		t.Fatalf("expected the entry to be updated in place, not duplicated: %+v", out.Profiles)
	}
	if got := out.Profiles["work"].URL; got != "https://new.example.com" {
		t.Errorf("work url = %q, want the updated value", got)
	}
}

func TestSetProfile_PreservesDefaultProfile(t *testing.T) {
	orig := &Config{DefaultProfile: "home", Profiles: map[string]Profile{}}
	out := SetProfile(orig, "work", Profile{URL: "https://work.example.com"})
	if out.DefaultProfile != "home" {
		t.Errorf("DefaultProfile = %q, want %q", out.DefaultProfile, "home")
	}
}

func TestRemoveProfile_RemovesEntry_DoesNotMutateOriginal(t *testing.T) {
	orig := &Config{Profiles: map[string]Profile{
		"home": {URL: "unix:///tmp/a.sock"},
		"work": {URL: "https://work.example.com"},
	}}
	out := RemoveProfile(orig, "work")

	if len(orig.Profiles) != 2 {
		t.Fatalf("RemoveProfile must not mutate its input; orig.Profiles = %+v", orig.Profiles)
	}
	if len(out.Profiles) != 1 {
		t.Fatalf("expected 1 profile remaining, got %d: %+v", len(out.Profiles), out.Profiles)
	}
	if _, ok := out.Profiles["work"]; ok {
		t.Error("expected \"work\" to be removed")
	}
	if _, ok := out.Profiles["home"]; !ok {
		t.Error("expected \"home\" to remain")
	}
}

func TestRemoveProfile_AbsentName_NoOp(t *testing.T) {
	orig := &Config{Profiles: map[string]Profile{"home": {URL: "unix:///tmp/a.sock"}}}
	out := RemoveProfile(orig, "ghost")
	if len(out.Profiles) != 1 {
		t.Fatalf("expected no-op for an absent name, got %+v", out.Profiles)
	}
}

// --- WriteConfig: on-disk round trip + preservation ---

func TestWriteConfig_MissingFile_CreatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boid", "config.yaml")
	cfg := &Config{
		DefaultProfile: "work",
		Profiles:       map[string]Profile{"work": {URL: "https://work.example.com"}},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.yaml perm = %o, want 0600", perm)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q, want %q", got.DefaultProfile, "work")
	}
	if u := got.Profiles["work"].URL; u != "https://work.example.com" {
		t.Errorf("work url = %q", u)
	}
}

// TestWriteConfig_PreservesUnrelatedTopLevelSections pins the single most
// important property of WriteConfig (docs/plans/cli-remote-connection.md
// "config.yaml の boid 管理外フィールドは preserve (最重要)"): writing a
// profile entry into a config.yaml that already has web/gc/gateway sections
// must leave those sections completely intact.
func TestWriteConfig_PreservesUnrelatedTopLevelSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := "web:\n" +
		"  public_url: https://boid.example.com\n" +
		"  http_addr: 127.0.0.1:8080\n" +
		"gc:\n" +
		"  enabled: true\n" +
		"gateway:\n" +
		"  forges:\n" +
		"    github: {}\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	newCfg := SetProfile(cfg, "home", Profile{URL: "unix:///run/user/1000/boid.sock"})
	newCfg.DefaultProfile = "home"
	if err := WriteConfig(path, newCfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	out := string(data)
	for _, want := range []string{
		"public_url: https://boid.example.com",
		"http_addr: 127.0.0.1:8080",
		"enabled: true",
		"github:",
		"default_profile: home",
		"unix:///run/user/1000/boid.sock",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("written config.yaml missing %q; got:\n%s", want, out)
		}
	}

	// Round-trip through LoadConfig too (not just a raw string contains
	// check) to make sure the gateway/gc/web sections still parse as valid
	// YAML alongside the new profiles section, not just as byte soup.
	roundTripped, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after WriteConfig: %v", err)
	}
	if roundTripped.DefaultProfile != "home" {
		t.Errorf("DefaultProfile = %q, want %q", roundTripped.DefaultProfile, "home")
	}
	if u := roundTripped.Profiles["home"].URL; u != "unix:///run/user/1000/boid.sock" {
		t.Errorf("home url = %q", u)
	}
}

func TestWriteConfig_EmptyProfiles_OmitsProfilesKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := "profiles:\n  work:\n    url: https://work.example.com\ndefault_profile: work\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	newCfg := RemoveProfile(cfg, "work")
	newCfg.DefaultProfile = ""
	if err := WriteConfig(path, newCfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	roundTripped, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after WriteConfig: %v", err)
	}
	if roundTripped.DefaultProfile != "" {
		t.Errorf("DefaultProfile = %q, want empty", roundTripped.DefaultProfile)
	}
	if len(roundTripped.Profiles) != 0 {
		t.Errorf("expected no profiles remaining, got %+v", roundTripped.Profiles)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if strings.Contains(string(data), "work.example.com") {
		t.Errorf("expected the removed profile's url to be gone entirely, got:\n%s", data)
	}
}

func TestWriteConfig_UpdatesExistingProfileURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := "profiles:\n  work:\n    url: https://old.example.com\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	newCfg := SetProfile(cfg, "work", Profile{URL: "https://new.example.com"})
	if err := WriteConfig(path, newCfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	roundTripped, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after WriteConfig: %v", err)
	}
	if u := roundTripped.Profiles["work"].URL; u != "https://new.example.com" {
		t.Errorf("work url = %q, want the updated value", u)
	}
}

func TestWriteConfig_MultipleProfiles_DeterministicOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{Profiles: map[string]Profile{
		"zzz": {URL: "https://zzz.example.com"},
		"aaa": {URL: "https://aaa.example.com"},
		"mmm": {URL: "https://mmm.example.com"},
	}}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	idxAaa := strings.Index(string(data), "aaa:")
	idxMmm := strings.Index(string(data), "mmm:")
	idxZzz := strings.Index(string(data), "zzz:")
	if !(idxAaa < idxMmm && idxMmm < idxZzz) {
		t.Errorf("expected sorted profile order aaa < mmm < zzz in output, got:\n%s", data)
	}
}

// --- MutateConfig: read-modify-write serialization ---

func TestMutateConfig_AppliesMutator_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	err := MutateConfig(path, func(cur *Config) (*Config, error) {
		out := SetProfile(cur, "work", Profile{URL: "https://work.example.com"})
		out.DefaultProfile = "work"
		return out, nil
	})
	if err != nil {
		t.Fatalf("MutateConfig: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Profiles["work"].URL != "https://work.example.com" {
		t.Errorf("profile URL = %q, want persisted", got.Profiles["work"].URL)
	}
	if got.DefaultProfile != "work" {
		t.Errorf("default_profile = %q, want %q", got.DefaultProfile, "work")
	}
}

func TestMutateConfig_MutatorReturnsNil_NoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// seed with a known config first.
	if err := WriteConfig(path, &Config{Profiles: map[string]Profile{"home": {URL: "unix:///tmp/a.sock"}}}); err != nil {
		t.Fatalf("seed WriteConfig: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	err = MutateConfig(path, func(_ *Config) (*Config, error) { return nil, nil })
	if err != nil {
		t.Fatalf("MutateConfig with nil-return mutator: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("config.yaml changed despite mutator returning nil:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMutateConfig_MutatorErrorPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	wantErr := fmt.Errorf("mutator sentinel")
	err := MutateConfig(path, func(_ *Config) (*Config, error) { return nil, wantErr })
	if err != wantErr {
		t.Errorf("MutateConfig error = %v, want the mutator's sentinel unchanged", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("mutator errored but config.yaml was created anyway")
	}
}

// TestMutateConfig_ConcurrentAppendsBothPersist pins the whole reason
// MutateConfig exists: two concurrent processes appending to config.yaml
// via LoadConfig → SetProfile → WriteConfig raced would lose one write
// (the second reader observes the first's pre-write state, then writes
// back a config missing what the first added). With MutateConfig's flock
// both entries survive.
//
// The start-barrier channel is DELIBERATE (codex PR2 round-2 minor): a
// bare sync.WaitGroup does not force all goroutines to be inside the
// MutateConfig read phase simultaneously — Go's default goroutine
// scheduler can happily interleave them serially, in which case even a
// broken (lock-less) implementation would pass this test. Blocking all
// N goroutines on a receive from the same channel and closing it once
// they are all parked releases them all at once, so the "N readers
// observing the same pre-write state before the first writer commits"
// race actually gets exercised — with the flock, mutations still
// serialize; without it, at least one insert loses.
func TestMutateConfig_ConcurrentAppendsBothPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const goroutines = 8
	var (
		wg    sync.WaitGroup
		ready sync.WaitGroup
	)
	start := make(chan struct{})
	wg.Add(goroutines)
	ready.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			name := fmt.Sprintf("p%d", i)
			_ = MutateConfig(path, func(cur *Config) (*Config, error) {
				return SetProfile(cur, name, Profile{URL: fmt.Sprintf("https://p%d.example.com", i)}), nil
			})
		}()
	}
	// Wait until every goroutine has parked on <-start, then release
	// them simultaneously so the race actually happens.
	ready.Wait()
	close(start)
	wg.Wait()

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Profiles) != goroutines {
		t.Errorf("got %d profiles after %d concurrent MutateConfig calls, want all %d to survive: %+v",
			len(got.Profiles), goroutines, goroutines, got.Profiles)
	}
}

func TestWriteConfig_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "boid", "config.yaml")
	cfg := &Config{Profiles: map[string]Profile{"home": {URL: "unix:///tmp/a.sock"}}}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config.yaml to exist: %v", err)
	}
}

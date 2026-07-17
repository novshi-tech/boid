package profiles

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/novshi-tech/boid/internal/config"
	"gopkg.in/yaml.v3"
)

// configLockName is the on-disk lock file that serializes concurrent
// `boid login`/`boid logout` (and any other future MutateConfig caller)
// so a stale in-memory Config value read before another writer commits
// can never overwrite that writer's changes. Kept as a sibling of
// config.yaml under ~/.config/boid so it lives on the same filesystem
// as the write it protects (advisory flock does not cross filesystems).
const configLockName = "config.lock"

// LockConfigMutation takes an exclusive advisory flock on the config.yaml
// mutation lock (a sibling config.lock file next to cfgPath) and returns
// a release function that MUST be called via defer. It is the shared
// serialization primitive for every writer that touches config.yaml —
// including profiles.MutateConfig (login/logout's profile mutations) and
// cmd/web.go's set-url/set-addr (web.public_url / web.addr mutations).
// All those writers hitting the same flock file guarantees a "read →
// modify → write" cycle in one process cannot interleave with another's
// and silently lose data (the whole reason MutateConfig existed to
// begin with — codex PR2 review round 2).
//
// The lock file itself is created 0600 in the config directory (which is
// created 0755 if missing). A caller is responsible for releasing:
//
//	release, err := profiles.LockConfigMutation(cfgPath)
//	if err != nil { return err }
//	defer release()
//	// … read config.yaml, mutate, atomic write …
func LockConfigMutation(cfgPath string) (release func(), err error) {
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("profiles: mkdir %q: %w", dir, err)
	}
	lockPath := filepath.Join(dir, configLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("profiles: open config lock %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("profiles: acquire config lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// MutateConfig serializes a read-modify-write cycle on cfgPath under
// LockConfigMutation, so two writers racing against the same config.yaml
// cannot lose each other's changes:
//
//   - acquire the shared config lock (LockConfigMutation)
//   - LoadConfig(cfgPath) — read the current on-disk state INSIDE the lock,
//     so the mutator never runs against a value that was already stale by
//     the time it was passed in
//   - call mutator(cfg) to compute the new Config value; the mutator may
//     also perform matching side-effect writes to sibling profile-scoped
//     files (token files — the mutator's `login` caller writes the token
//     inside this lock so no concurrent login on the SAME profile can end
//     up with a token file whose URL disagrees with the config entry
//     written next; codex PR2 review round 2)
//   - WriteConfig(cfgPath, newCfg) atomically (temp+rename), still inside
//     the lock, so no other process observes an intermediate state
//   - release the flock on return
//
// mutator must not retain cfg after returning — the caller is expected to
// derive newCfg from it (SetProfile / RemoveProfile both return copies)
// and let cfg fall out of scope. A nil newCfg means "no write needed"
// (idempotent path, e.g. logout on a profile that was already absent by
// the time we grabbed the lock).
func MutateConfig(cfgPath string, mutator func(*Config) (*Config, error)) error {
	release, err := LockConfigMutation(cfgPath)
	if err != nil {
		return err
	}
	defer release()

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	newCfg, err := mutator(cfg)
	if err != nil {
		return err
	}
	if newCfg == nil {
		return nil
	}
	return WriteConfig(cfgPath, newCfg)
}

// SetProfile returns a copy of cfg with name's entry added or updated to
// prof. cfg itself is never mutated — callers (cmd/login.go) hold onto the
// value LoadConfig returned them for diagnostics (e.g. "already exists")
// and must not have it silently change underneath them.
func SetProfile(cfg *Config, name string, prof Profile) *Config {
	out := &Config{DefaultProfile: cfg.DefaultProfile}
	out.Profiles = make(map[string]Profile, len(cfg.Profiles)+1)
	for k, v := range cfg.Profiles {
		out.Profiles[k] = v
	}
	out.Profiles[name] = prof
	return out
}

// RemoveProfile returns a copy of cfg with name's entry removed. Removing a
// name that is not present is a no-op (idempotent — cmd/login.go's
// `boid logout` relies on this to be safe to call twice).
func RemoveProfile(cfg *Config, name string) *Config {
	out := &Config{DefaultProfile: cfg.DefaultProfile}
	if len(cfg.Profiles) == 0 {
		return out
	}
	out.Profiles = make(map[string]Profile, len(cfg.Profiles))
	for k, v := range cfg.Profiles {
		if k == name {
			continue
		}
		out.Profiles[k] = v
	}
	return out
}

// WriteConfig serializes cfg's default_profile/profiles fields into path
// (~/.config/boid/config.yaml) and writes it atomically (temp file in the
// same directory, fsynced, renamed into place — config.WriteFileAtomic),
// permission 0600. The parent directory is created (0755, matching the
// existing `boid web set-url`/`set-addr` convention in cmd/web.go) if it
// does not exist yet.
//
// The single most important property of this function is what it does NOT
// touch: config.yaml is a file shared with internal/config.Config's own
// gc/web/notify/sandbox/task_ask/gateway sections (see Config's own doc
// comment in config.go), and a real dogfood config.yaml already has some of
// those. A naive "unmarshal whole file into a generic map, mutate
// default_profile/profiles, marshal the whole map back out" round-trip is
// exactly what this function must NOT do (map key/value order is not
// preserved by Go's map type, so every unrelated section would get silently
// reformatted — style-only churn on a first write, but map ordering makes
// even that non-deterministic across runs).
//
// Instead this reads the file as a yaml.Node tree (loadConfigMappingNode),
// surgically replaces (or removes) only the "default_profile" and
// "profiles" entries within the top-level mapping via mapNodeSetOrRemove,
// and marshals that same tree back out — so every other top-level key
// (web, gc, gateway, ...) round-trips as the exact same node structure it
// was parsed from, byte-identical modulo yaml.v3's own re-indentation
// (which does not touch semantics — the values are unchanged Nodes, not
// re-derived from a lossy Go value).
func WriteConfig(path string, cfg *Config) error {
	if cfg == nil {
		cfg = &Config{}
	}
	root, err := loadConfigMappingNode(path)
	if err != nil {
		return err
	}
	applyConfigToNode(root, cfg)

	data, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("profiles: marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("profiles: mkdir %q: %w", filepath.Dir(path), err)
	}
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("profiles: write config %q: %w", path, err)
	}
	return nil
}

// loadConfigMappingNode reads path and returns its top-level YAML mapping
// node, ready for in-place mutation by applyConfigToNode. A missing file or
// an empty/whitespace-only one is not an error — it returns a fresh, empty
// mapping node (mirrors LoadConfig's "missing file → empty Config"
// contract), so `boid login` works on a brand-new machine with no
// config.yaml yet.
//
// gopkg.in/yaml.v3 unmarshals a document into a *yaml.Node as a DocumentNode
// wrapping the real content as Content[0] (verified empirically — see the
// package's write_test.go for a pinning test); this unwraps that one level
// so every other helper in this file only ever deals with a plain
// MappingNode, never a DocumentNode.
func loadConfigMappingNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newEmptyMappingNode(), nil
		}
		return nil, fmt.Errorf("profiles: read %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return newEmptyMappingNode(), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("profiles: parse %q: %w", path, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return newEmptyMappingNode(), nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("profiles: %q: root is not a YAML mapping", path)
	}
	return root, nil
}

func newEmptyMappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

// applyConfigToNode mutates root (a top-level YAML mapping node) in place so
// its "default_profile" and "profiles" entries match cfg — added, updated,
// or removed as needed — while leaving every other key in root untouched.
func applyConfigToNode(root *yaml.Node, cfg *Config) {
	if cfg.DefaultProfile == "" {
		mapNodeSetOrRemove(root, "default_profile", nil)
	} else {
		mapNodeSetOrRemove(root, "default_profile", scalarStrNode(cfg.DefaultProfile))
	}
	if len(cfg.Profiles) == 0 {
		mapNodeSetOrRemove(root, "profiles", nil)
	} else {
		mapNodeSetOrRemove(root, "profiles", profilesMapNode(cfg.Profiles))
	}
}

// mapNodeSetOrRemove finds key within m's flat Content slice (alternating
// key/value node pairs — the yaml.v3 MappingNode representation) and either
// replaces its value (value != nil) or deletes the key/value pair entirely
// (value == nil). Setting a key that is not yet present appends a new pair;
// removing a key that is not present is a silent no-op. Every other pair in
// m.Content is left untouched, in its original order and with its original
// Node objects — this is the mechanism that makes WriteConfig's "preserve
// unrelated top-level fields" contract hold.
func mapNodeSetOrRemove(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if value == nil {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
			} else {
				m.Content[i+1] = value
			}
			return
		}
	}
	if value == nil {
		return
	}
	m.Content = append(m.Content, scalarStrNode(key), value)
}

// profilesMapNode builds the "profiles:" mapping node for a Config.Profiles
// map. Keys are emitted in sorted order so the on-disk output is
// deterministic across writes (Go map iteration order is randomized, and a
// nondeterministic config.yaml diff on every `boid login`/`boid logout`
// would be an unpleasant surprise for a human inspecting it, or a dotfiles
// repo tracking it).
func profilesMapNode(profilesMap map[string]Profile) *yaml.Node {
	names := make([]string, 0, len(profilesMap))
	for name := range profilesMap {
		names = append(names, name)
	}
	sort.Strings(names)

	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, name := range names {
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = append(entry.Content, scalarStrNode("url"), scalarStrNode(profilesMap[name].URL))
		m.Content = append(m.Content, scalarStrNode(name), entry)
	}
	return m
}

func scalarStrNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

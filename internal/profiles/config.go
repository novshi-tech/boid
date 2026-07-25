// Package profiles resolves which boid daemon a CLI invocation should talk
// to: the local UNIX socket (today's sole, and still default, behavior) or
// a remote daemon over HTTPS + Bearer auth, per a named "profile" in
// ~/.config/boid/config.yaml (docs/plans/cli-remote-connection.md, Phase 3
// PR1 "profile 基盤").
package profiles

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Profile is a single named connection target from config.yaml's
// `profiles:` map. Deliberately does not carry a token field: the token
// lives in ~/.config/boid/tokens/<profile>.json (token.go), never in
// config.yaml — see decision 2 (docs/plans/cli-remote-connection.md).
type Profile struct {
	URL string `yaml:"url"`
}

// Config is the profile-related subset of ~/.config/boid/config.yaml.
// config.yaml is a single shared file with several unrelated top-level
// sections (internal/config.Config's own gc/web/notify/sandbox/task_ask/
// gateway) — see UnmarshalYAML's doc comment for why this type's decoding
// deliberately does NOT reject those siblings.
type Config struct {
	// DefaultProfile is used when neither --profile nor BOID_PROFILE is
	// set. Empty means "no default" — Resolve then falls back to the
	// pre-Phase-3 unix-socket default (docs/plans/cli-remote-connection.md's
	// "現行互換" contract).
	DefaultProfile string `yaml:"default_profile"`
	// Profiles maps a profile name (validated by ValidateSlug wherever a
	// name is actually selected — this type itself does not enforce it, so
	// a hand-edited config.yaml with an invalid key still parses and
	// Resolve surfaces the slug error with full context) to its connection
	// target.
	Profiles map[string]Profile `yaml:"profiles"`
}

// UnmarshalYAML implements yaml.Unmarshaler. It intentionally decodes ONLY
// the default_profile/profiles keys and silently ignores every other
// top-level key in the document — mirroring internal/config.Config's own
// UnmarshalYAML ("Unknown legacy fields... are silently ignored"), for the
// same reason: config.yaml is shared by several independent loaders (gc,
// web, gateway, sandbox, task_ask, and now profiles), and a plain
// dec.KnownFields(true) applied to the whole document would make this
// package reject any config.yaml that ALSO configures, say,
// gateway.forges or web.public_url — which is not "strict validation", it
// is a hard crash on a file every other loader in this codebase treats as
// perfectly normal.
//
// What this DOES validate strictly is the shape *within* each
// profiles.<name> entry: a bare yaml.Node.Decode call always starts a
// fresh, loose decode regardless of the Decoder that produced the node
// (see internal/orchestrator/host_commands_config.go's
// hostCommandSpecStrict doc comment for the identical gopkg.in/yaml.v3
// quirk, applied there one level down for the same reason) — so each
// profile's raw Node is re-marshaled to its own small YAML document and
// re-decoded through its own KnownFields(true) Decoder below, which does
// catch a typo'd field (e.g. "urll:") instead of silently dropping it.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		DefaultProfile string               `yaml:"default_profile"`
		Profiles       map[string]yaml.Node `yaml:"profiles"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.DefaultProfile = raw.DefaultProfile
	if len(raw.Profiles) == 0 {
		c.Profiles = nil
		return nil
	}
	c.Profiles = make(map[string]Profile, len(raw.Profiles))
	for name, node := range raw.Profiles {
		node := node // capture for &node below (pre-Go-1.22 loop var safety; harmless either way)
		data, err := yaml.Marshal(&node)
		if err != nil {
			return fmt.Errorf("profiles.%s: %w", name, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var p Profile
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("profiles.%s: %w", name, err)
		}
		c.Profiles[name] = p
	}
	return nil
}

// ConfigPath returns ~/.config/boid/config.yaml, honoring $XDG_CONFIG_HOME
// (os.UserConfigDir does this natively on Linux) — the same file
// `boid web set-url`/`boid web set-addr` (cmd/web.go) already read-merge-
// write, and the same resolution strategy internal/config.Load and
// internal/orchestrator.DefaultHostCommandsPath use for their own files
// under the same directory.
func ConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("profiles: could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "config.yaml"), nil
}

// LoadConfig reads and parses path. A missing file is not an error — it
// returns an empty Config, matching the documented "config.yaml が存在しない"
// branch of Resolve's 現行互換 fallback (docs/plans/cli-remote-connection.md).
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("profiles: read %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &Config{}, nil
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("profiles: parse %q: %w", path, err)
	}
	return &cfg, nil
}

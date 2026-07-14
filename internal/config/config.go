package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"gopkg.in/yaml.v3"
)

// Config holds the global boid configuration.
type Config struct {
	GC      GCConfig      `yaml:"gc"`
	Web     WebConfig     `yaml:"web"`
	Notify  NotifyConfig  `yaml:"notify"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	TaskAsk TaskAskConfig `yaml:"task_ask"`
	Gateway GatewayConfig `yaml:"gateway"`

	// DefaultHarness is the harness identifier (claude, codex, opencode, ...)
	// used by sub-commands that need to launch an interactive agent without
	// a project-level harness context (e.g. `boid kit init`). Empty means
	// "ask the user on first use" — see DefaultHarness() for the resolver.
	DefaultHarness string `yaml:"default_harness"`
}

// TaskAskConfig holds settings for the blocking `boid task ask` Q&A RPC.
type TaskAskConfig struct {
	// DisconnectGrace is how long an awaiting task may sit with no live agent
	// parked (the agent's `boid task ask` was killed by a harness command-timeout
	// and disconnected) before the daemon reclaims it. The agent normally
	// re-asks within one command-timeout and re-attaches; the grace bounds the
	// case where it never returns.
	DisconnectGrace time.Duration `yaml:"-"`
}

// SandboxConfig holds sandbox-related settings.
type SandboxConfig struct {
	AllowedDomains []string `yaml:"allowed_domains"`
}

// ForgeConfig configures the git gateway's credential injection for a
// single forge id (the map key in GatewayConfig.Forges). Only the forge
// kind and a secret-store key reference are ever written here — the
// plaintext PAT itself lives in the secret store (`boid secret set <key>
// <value>`), never in config.yaml.
//
// Built-in ids ("github", "bitbucket") default every field left empty here
// (see builtinForges): host, Basic-auth forge convention, and secret-store
// key all resolve without the user writing anything, so `gateway.forges:
// {github: {}}` — or omitting the id entirely, since DefaultConfig
// pre-populates both built-ins — is enough for `boid secret set github-pat
// <PAT>` to light up the gateway for github.com. Custom ids (e.g.
// "github-enterprise") must set Host explicitly, and Forge must name one of
// gitgateway's recognized conventions since that convention is not itself
// derivable from an arbitrary id.
type ForgeConfig struct {
	// Host is the upstream host as it appears in the gateway route path
	// (e.g. "github.com"). Optional for built-in ids; required otherwise.
	Host string `yaml:"host,omitempty"`
	// Forge selects the Basic-auth username convention
	// (gitgateway.ForgeGitHub / gitgateway.ForgeBitbucket). Optional for
	// built-in ids; required otherwise.
	Forge gitgateway.Forge `yaml:"forge,omitempty"`
	// SecretKey is a reference into the secret store
	// (internal/dispatcher/secret_store.go); never a plaintext token.
	// Optional for built-in ids (defaults below); required otherwise.
	SecretKey string `yaml:"secret_key,omitempty"`
}

// GatewayConfig configures the git gateway's per-forge credential injection
// (post-cutover §2: config surface を forges map に圧縮 + github/bitbucket
// を内蔵デフォルト化). Forges maps a forge id to its credential config;
// Config.UnmarshalYAML also accepts the deprecated pre-forges-map
// `gateway.hosts` list (docs/plans/git-gateway-cutover.md PR4's original
// schema) and folds it into this map, so GatewayConfig itself only ever
// needs to carry the one shape.
type GatewayConfig struct {
	// Forges maps a forge id (e.g. "github", "bitbucket", or a custom id
	// like "github-enterprise") to its credential config. Built-in ids
	// "github" and "bitbucket" are pre-populated by DefaultConfig with
	// host/forge/secret_key defaults already filled in — see builtinForges.
	Forges map[string]ForgeConfig `yaml:"forges,omitempty"`
}

// builtinForges lists the forge ids DefaultConfig pre-populates and the
// defaults resolveForgeConfig fills in for any field a built-in id's
// ForgeConfig leaves empty.
var builtinForges = map[string]gitgateway.HostForgeConfig{
	"github":    {Host: "github.com", Forge: gitgateway.ForgeGitHub, SecretKey: "github-pat"},
	"bitbucket": {Host: "bitbucket.org", Forge: gitgateway.ForgeBitbucket, SecretKey: "bitbucket-token"},
}

// resolveForgeConfig fills in built-in defaults (when id names one of
// builtinForges) and validates the result, returning the fully-resolved
// gitgateway.HostForgeConfig for a single gateway.forges entry.
func resolveForgeConfig(id string, fc ForgeConfig) (gitgateway.HostForgeConfig, error) {
	h := gitgateway.HostForgeConfig{Host: fc.Host, Forge: fc.Forge, SecretKey: fc.SecretKey}
	if def, ok := builtinForges[id]; ok {
		if h.Host == "" {
			h.Host = def.Host
		}
		if h.Forge == "" {
			h.Forge = def.Forge
		}
		if h.SecretKey == "" {
			h.SecretKey = def.SecretKey
		}
	}
	if h.Host == "" {
		return gitgateway.HostForgeConfig{}, fmt.Errorf(
			"gateway.forges[%q]: missing required \"host\" field (only built-in ids %q/%q default it)",
			id, "github", "bitbucket")
	}
	if h.SecretKey == "" {
		return gitgateway.HostForgeConfig{}, fmt.Errorf("gateway.forges[%q]: missing required \"secret_key\" field", id)
	}
	switch h.Forge {
	case gitgateway.ForgeGitHub, gitgateway.ForgeBitbucket:
	default:
		return gitgateway.HostForgeConfig{}, fmt.Errorf("gateway.forges[%q]: unrecognized forge %q (want %q or %q)",
			id, h.Forge, gitgateway.ForgeGitHub, gitgateway.ForgeBitbucket)
	}
	return h, nil
}

// HostConfigs resolves g.Forges into the flat gitgateway.HostForgeConfig
// list gitgateway.NewCredentialProvider consumes (internal/server/wire.go),
// applying built-in defaults per resolveForgeConfig. Entries are returned in
// id-sorted order for determinism. g is assumed already validated —
// Config.UnmarshalYAML validates every entry at load time — so a resolution
// failure here is skipped defensively rather than surfaced as an error.
func (g GatewayConfig) HostConfigs() []gitgateway.HostForgeConfig {
	ids := make([]string, 0, len(g.Forges))
	for id := range g.Forges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]gitgateway.HostForgeConfig, 0, len(ids))
	for _, id := range ids {
		if h, err := resolveForgeConfig(id, g.Forges[id]); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// NotifyConfig holds settings for agent-driven notifications.
type NotifyConfig struct {
	Command []string `yaml:"command"`
}

// GCConfig holds garbage collection settings.
type GCConfig struct {
	Enabled   bool          `yaml:"-"`
	Interval  time.Duration `yaml:"-"`
	OlderThan time.Duration `yaml:"-"`
}

// WebConfig holds web UI settings.
type WebConfig struct {
	PublicURL string `yaml:"public_url"`
	HTTPAddr  string `yaml:"http_addr"`
}

// DefaultConfig returns the default boid configuration.
func DefaultConfig() *Config {
	return &Config{
		GC: GCConfig{
			Enabled:   true,
			Interval:  24 * time.Hour,
			OlderThan: 720 * time.Hour,
		},
		TaskAsk: TaskAskConfig{
			DisconnectGrace: 30 * time.Minute,
		},
		Gateway: GatewayConfig{
			// Built in so `boid secret set github-pat <PAT>` (or
			// bitbucket-token) lights up the gateway with zero
			// config.yaml edits — see ForgeConfig's doc comment.
			Forges: map[string]ForgeConfig{
				"github":    {},
				"bitbucket": {},
			},
		},
	}
}

// Load reads the configuration from the default XDG path (~/.config/boid/config.yaml).
// If the file does not exist, the default configuration is returned without error.
func Load() (*Config, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return DefaultConfig(), nil
	}
	path := filepath.Join(configDir, "boid", "config.yaml")
	return loadFromPath(path)
}

// loadFromPath reads the configuration from the given path.
// If the file does not exist, the default configuration is returned without error.
func loadFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// UnmarshalYAML implements yaml.Unmarshaler for Config.
// It starts from DefaultConfig values so that unspecified fields retain defaults.
// Unknown legacy fields (state_machine.rework_limit) are silently ignored.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	defaults := DefaultConfig()

	var raw struct {
		GC struct {
			Enabled   *bool  `yaml:"enabled"`
			Interval  string `yaml:"interval"`
			OlderThan string `yaml:"older_than"`
		} `yaml:"gc"`
		Web struct {
			PublicURL string `yaml:"public_url"`
			HTTPAddr  string `yaml:"http_addr"`
		} `yaml:"web"`
		Notify struct {
			Command []string `yaml:"command"`
		} `yaml:"notify"`
		Sandbox struct {
			AllowedDomains []string `yaml:"allowed_domains"`
		} `yaml:"sandbox"`
		TaskAsk struct {
			DisconnectGrace string `yaml:"disconnect_grace"`
		} `yaml:"task_ask"`
		Gateway struct {
			Forges map[string]ForgeConfig `yaml:"forges"`
			// Hosts is the deprecated pre-forges-map schema
			// (docs/plans/git-gateway-cutover.md PR4). Still parsed for
			// one release as a compatibility shim — see the Gateway
			// handling below, which logs a deprecation warning and folds
			// it into Forges.
			//
			// Deprecated: use Forges.
			Hosts []gitgateway.HostForgeConfig `yaml:"hosts"`
		} `yaml:"gateway"`
		DefaultHarness string `yaml:"default_harness"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	c.GC = defaults.GC
	c.TaskAsk = defaults.TaskAsk

	if raw.GC.Enabled != nil {
		c.GC.Enabled = *raw.GC.Enabled
	}
	if raw.GC.Interval != "" {
		d, err := time.ParseDuration(raw.GC.Interval)
		if err != nil {
			return fmt.Errorf("gc.interval: %w", err)
		}
		c.GC.Interval = d
	}
	if raw.GC.OlderThan != "" {
		d, err := time.ParseDuration(raw.GC.OlderThan)
		if err != nil {
			return fmt.Errorf("gc.older_than: %w", err)
		}
		c.GC.OlderThan = d
	}

	c.Web.PublicURL = raw.Web.PublicURL
	c.Web.HTTPAddr = raw.Web.HTTPAddr

	c.Notify.Command = raw.Notify.Command

	c.Sandbox.AllowedDomains = raw.Sandbox.AllowedDomains

	if raw.TaskAsk.DisconnectGrace != "" {
		d, err := time.ParseDuration(raw.TaskAsk.DisconnectGrace)
		if err != nil {
			return fmt.Errorf("task_ask.disconnect_grace: %w", err)
		}
		c.TaskAsk.DisconnectGrace = d
	}

	// Start from the built-in defaults (github/bitbucket), then let the
	// user's gateway.forges entries override/extend them by id.
	forges := make(map[string]ForgeConfig, len(defaults.Gateway.Forges)+len(raw.Gateway.Forges))
	for id, fc := range defaults.Gateway.Forges {
		forges[id] = fc
	}
	for id, fc := range raw.Gateway.Forges {
		forges[id] = fc
	}

	// Resolve gateway.forges first (fully validating it) so the legacy
	// gateway.hosts loop below can apply the "forges > hosts" priority rule
	// per-host.
	resolvedHosts := make(map[string]struct{}, len(forges))
	for id, fc := range forges {
		h, err := resolveForgeConfig(id, fc)
		if err != nil {
			return err
		}
		resolvedHosts[h.Host] = struct{}{}
	}

	if len(raw.Gateway.Hosts) > 0 {
		slog.Warn("gateway.hosts is deprecated and will be removed in a future release; use gateway.forges instead " +
			"(see docs/ja/reference/config-yaml.md#gateway--git-gateway)")
		for _, h := range raw.Gateway.Hosts {
			if h.Host == "" {
				return fmt.Errorf("gateway.hosts: entry missing required \"host\" field")
			}
			if h.SecretKey == "" {
				return fmt.Errorf("gateway.hosts: host %q: missing required \"secret_key\" field", h.Host)
			}
			switch h.Forge {
			case gitgateway.ForgeGitHub, gitgateway.ForgeBitbucket:
			default:
				return fmt.Errorf("gateway.hosts: host %q: unrecognized forge %q (want %q or %q)",
					h.Host, h.Forge, gitgateway.ForgeGitHub, gitgateway.ForgeBitbucket)
			}
			if _, dup := resolvedHosts[h.Host]; dup {
				slog.Warn("gateway.hosts entry ignored: host is already configured via gateway.forges", "host", h.Host)
				continue
			}
			// Use the host itself as the synthetic forges id: legacy
			// entries already carry host/forge/secret_key fully resolved,
			// so no built-in defaulting ever applies to them.
			forges[h.Host] = ForgeConfig{Host: h.Host, Forge: h.Forge, SecretKey: h.SecretKey}
			resolvedHosts[h.Host] = struct{}{}
		}
	}
	c.Gateway = GatewayConfig{Forges: forges}

	c.DefaultHarness = raw.DefaultHarness

	return nil
}

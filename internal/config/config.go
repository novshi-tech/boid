package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the global boid configuration.
type Config struct {
	GC      GCConfig      `yaml:"gc"`
	Web     WebConfig     `yaml:"web"`
	Notify  NotifyConfig  `yaml:"notify"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	TaskAsk TaskAskConfig `yaml:"task_ask"`

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

	c.DefaultHarness = raw.DefaultHarness

	return nil
}

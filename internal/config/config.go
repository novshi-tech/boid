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
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	c.GC = defaults.GC

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

	return nil
}

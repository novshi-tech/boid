package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/gitgateway"
)

// captureSlog redirects the default slog logger to an in-memory buffer for
// the duration of the test. Helper for verifying deprecation warnings.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestLoadFromPath_FileNotExist_ReturnsDefaults(t *testing.T) {
	cfg, err := loadFromPath(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := DefaultConfig()
	if cfg.GC.Enabled != def.GC.Enabled {
		t.Errorf("Enabled: got %v, want %v", cfg.GC.Enabled, def.GC.Enabled)
	}
	if cfg.GC.Interval != def.GC.Interval {
		t.Errorf("Interval: got %v, want %v", cfg.GC.Interval, def.GC.Interval)
	}
	if cfg.GC.OlderThan != def.GC.OlderThan {
		t.Errorf("OlderThan: got %v, want %v", cfg.GC.OlderThan, def.GC.OlderThan)
	}
}

func TestLoadFromPath_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gc:
  enabled: false
  interval: 12h
  older_than: 360h
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GC.Enabled != false {
		t.Errorf("Enabled: got %v, want false", cfg.GC.Enabled)
	}
	if cfg.GC.Interval != 12*time.Hour {
		t.Errorf("Interval: got %v, want 12h", cfg.GC.Interval)
	}
	if cfg.GC.OlderThan != 360*time.Hour {
		t.Errorf("OlderThan: got %v, want 360h", cfg.GC.OlderThan)
	}
}

func TestLoadFromPath_TaskAskDisconnectGrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
task_ask:
  disconnect_grace: 45m
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskAsk.DisconnectGrace != 45*time.Minute {
		t.Errorf("DisconnectGrace: got %v, want 45m", cfg.TaskAsk.DisconnectGrace)
	}
}

// An unset task_ask falls back to the 30m default.
func TestLoadFromPath_TaskAskDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc:\n  interval: 6h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskAsk.DisconnectGrace != 30*time.Minute {
		t.Errorf("DisconnectGrace default: got %v, want 30m", cfg.TaskAsk.DisconnectGrace)
	}
}

func TestLoadFromPath_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc: [invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadFromPath(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadFromPath_PartialYAML_UsesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// only set interval, others should be defaults
	content := `
gc:
  interval: 6h
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := DefaultConfig()
	if cfg.GC.Enabled != def.GC.Enabled {
		t.Errorf("Enabled: got %v, want %v (default)", cfg.GC.Enabled, def.GC.Enabled)
	}
	if cfg.GC.Interval != 6*time.Hour {
		t.Errorf("Interval: got %v, want 6h", cfg.GC.Interval)
	}
	if cfg.GC.OlderThan != def.GC.OlderThan {
		t.Errorf("OlderThan: got %v, want %v (default)", cfg.GC.OlderThan, def.GC.OlderThan)
	}
}

func TestLoadFromPath_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gc:
  interval: notaduration
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadFromPath(path)
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

// hostConfig looks up a single resolved gitgateway.HostForgeConfig by host
// from GatewayConfig.HostConfigs(), failing the test if it is absent.
func hostConfig(t *testing.T, cfg *Config, host string) gitgateway.HostForgeConfig {
	t.Helper()
	for _, h := range cfg.Gateway.HostConfigs() {
		if h.Host == host {
			return h
		}
	}
	t.Fatalf("HostConfigs() has no entry for host %q; got %#v", host, cfg.Gateway.HostConfigs())
	return gitgateway.HostForgeConfig{}
}

// An unset gateway block must not error, and the built-in github/bitbucket
// forges must resolve out of the box — the whole point of the post-cutover
// §2 default-embedding is that a brand new user gets a working gateway from
// `boid secret set github-pat <PAT>` alone, with zero config.yaml edits.
func TestLoadFromPath_Gateway_UnsetUsesBuiltinDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc:\n  interval: 6h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hosts := cfg.Gateway.HostConfigs()
	if len(hosts) != 2 {
		t.Fatalf("HostConfigs() = %#v, want 2 built-in entries", hosts)
	}
	gh := hostConfig(t, cfg, "github.com")
	if gh.Forge != gitgateway.ForgeGitHub || gh.SecretKey != "github-pat" {
		t.Errorf("github.com entry = %#v, want forge=github secret_key=github-pat", gh)
	}
	bb := hostConfig(t, cfg, "bitbucket.org")
	if bb.Forge != gitgateway.ForgeBitbucket || bb.SecretKey != "bitbucket-token" {
		t.Errorf("bitbucket.org entry = %#v, want forge=bitbucket secret_key=bitbucket-token", bb)
	}
}

func TestDefaultConfig_GatewayBuiltins(t *testing.T) {
	cfg := DefaultConfig()
	hosts := cfg.Gateway.HostConfigs()
	if len(hosts) != 2 {
		t.Fatalf("DefaultConfig().Gateway.HostConfigs() = %#v, want 2 built-in entries", hosts)
	}
}

// gateway.forges is the new (post-cutover §2) schema: a map keyed by forge
// id instead of a host/forge/secret_key triple list. Built-in ids only need
// a secret_key override; host and forge convention default.
func TestLoadFromPath_GatewayForges_OverrideBuiltinSecretKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github:
      secret_key: gh-pat
    bitbucket:
      secret_key: bb-token
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gh := hostConfig(t, cfg, "github.com")
	if gh.Forge != gitgateway.ForgeGitHub || gh.SecretKey != "gh-pat" {
		t.Errorf("github.com entry = %#v, want forge=github secret_key=gh-pat", gh)
	}
	bb := hostConfig(t, cfg, "bitbucket.org")
	if bb.Forge != gitgateway.ForgeBitbucket || bb.SecretKey != "bb-token" {
		t.Errorf("bitbucket.org entry = %#v, want forge=bitbucket secret_key=bb-token", bb)
	}
	// config.yaml never carries a plaintext token; Scheme is a test-only
	// override (yaml:"-") and must stay unset from a real config file.
	if gh.Scheme != "" {
		t.Errorf("github.com entry Scheme = %q, want empty (not yaml-settable)", gh.Scheme)
	}
}

// A custom (non-built-in) forge id, e.g. for a GitHub Enterprise host, must
// declare host/forge/secret_key explicitly since none of them can be
// derived from an arbitrary id.
func TestLoadFromPath_GatewayForges_CustomID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github-enterprise:
      host: github.corp.example.com
      forge: github
      secret_key: ghe-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ghe := hostConfig(t, cfg, "github.corp.example.com")
	if ghe.Forge != gitgateway.ForgeGitHub || ghe.SecretKey != "ghe-pat" {
		t.Errorf("github.corp.example.com entry = %#v, want forge=github secret_key=ghe-pat", ghe)
	}
	// Built-in defaults must still be present alongside the custom id.
	if len(cfg.Gateway.HostConfigs()) != 3 {
		t.Errorf("HostConfigs() = %#v, want 3 entries (2 built-in + 1 custom)", cfg.Gateway.HostConfigs())
	}
}

func TestLoadFromPath_GatewayForges_CustomIDMissingHostRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github-enterprise:
      forge: github
      secret_key: ghe-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for missing host on custom id, got nil")
	}
}

func TestLoadFromPath_GatewayForges_CustomIDMissingSecretKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github-enterprise:
      host: github.corp.example.com
      forge: github
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for missing secret_key on custom id, got nil")
	}
}

func TestLoadFromPath_GatewayForges_UnrecognizedForgeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    gitlab:
      host: gitlab.com
      forge: gitlab
      secret_key: gl-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for unrecognized forge, got nil")
	}
}

// The deprecated gateway.hosts list (docs/plans/git-gateway-cutover.md PR4's
// original schema) must keep working for one release, folded into the same
// resolved HostConfigs() list, while logging a deprecation warning.
func TestLoadFromPath_GatewayHosts_LegacyStillParses(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: gitlab.example.com
      forge: github
      secret_key: gl-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gl := hostConfig(t, cfg, "gitlab.example.com")
	if gl.Forge != gitgateway.ForgeGitHub || gl.SecretKey != "gl-pat" {
		t.Errorf("gitlab.example.com entry = %#v, want forge=github secret_key=gl-pat", gl)
	}
	// Built-in defaults must still resolve alongside the legacy entry.
	if len(cfg.Gateway.HostConfigs()) != 3 {
		t.Errorf("HostConfigs() = %#v, want 3 entries (2 built-in + 1 legacy)", cfg.Gateway.HostConfigs())
	}
	if !bytes.Contains(buf.Bytes(), []byte("gateway.hosts is deprecated")) {
		t.Errorf("expected a gateway.hosts deprecation warning, got log output: %s", buf.String())
	}
}

func TestLoadFromPath_GatewayHosts_UnrecognizedForgeRejected(t *testing.T) {
	captureSlog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: gitlab.com
      forge: gitlab
      secret_key: gl-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for unrecognized forge, got nil")
	}
}

func TestLoadFromPath_GatewayHosts_MissingSecretKeyRejected(t *testing.T) {
	captureSlog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: github.com
      forge: github
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for missing secret_key, got nil")
	}
}

// When both the new gateway.forges and the deprecated gateway.hosts
// configure the same host, forges must win and the hosts entry must be
// ignored (with a warning), per the "forges > hosts" priority rule.
func TestLoadFromPath_Gateway_ForgesOverridesHostsForSameHost(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github:
      secret_key: from-forges
  hosts:
    - host: github.com
      forge: github
      secret_key: from-hosts
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gh := hostConfig(t, cfg, "github.com")
	if gh.SecretKey != "from-forges" {
		t.Errorf("github.com entry SecretKey = %q, want %q (forges must win)", gh.SecretKey, "from-forges")
	}
	if len(cfg.Gateway.HostConfigs()) != 2 {
		t.Errorf("HostConfigs() = %#v, want 2 entries (github.com deduped, bitbucket.org built-in)", cfg.Gateway.HostConfigs())
	}
	if !bytes.Contains(buf.Bytes(), []byte("already configured via gateway.forges")) {
		t.Errorf("expected a warning that the gateway.hosts entry was ignored, got log output: %s", buf.String())
	}
}

// Regression: nose's real ~/.config/boid/config.yaml sets github.com's
// secret_key via the legacy hosts: form (e.g. GH_TOKEN, not the built-in
// default github-pat). Before this fix the built-in default seed collided
// with the legacy entry in the dup-check and silently discarded the
// legacy secret_key — every credential lookup then went to the built-in
// default key, missed, and fell open. The legacy hosts entry MUST override
// the built-in slot's secret_key when the user hasn't explicitly configured
// that built-in id via gateway.forges. See UnmarshalYAML's "byte-for-byte
// legacy compat" comment.
func TestLoadFromPath_GatewayHosts_LegacyPreservesBuiltinGitHubSecretKey(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: github.com
      forge: github
      secret_key: GH_TOKEN
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gh := hostConfig(t, cfg, "github.com")
	if gh.SecretKey != "GH_TOKEN" {
		t.Errorf("github.com SecretKey = %q, want %q (legacy hosts value must win over built-in default \"github-pat\")",
			gh.SecretKey, "GH_TOKEN")
	}
	if gh.Forge != gitgateway.ForgeGitHub {
		t.Errorf("github.com Forge = %q, want %q", gh.Forge, gitgateway.ForgeGitHub)
	}
	// bitbucket built-in must still resolve (untouched by the github override).
	bb := hostConfig(t, cfg, "bitbucket.org")
	if bb.SecretKey != "bitbucket-token" {
		t.Errorf("bitbucket.org SecretKey = %q, want built-in default %q", bb.SecretKey, "bitbucket-token")
	}
	if !bytes.Contains(buf.Bytes(), []byte("merged into built-in forge slot")) {
		t.Errorf("expected a warning that the legacy entry was merged into the built-in slot, got: %s", buf.String())
	}
}

func TestLoadFromPath_GatewayHosts_LegacyPreservesBuiltinBitbucketSecretKey(t *testing.T) {
	captureSlog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: bitbucket.org
      forge: bitbucket
      secret_key: BB_TOKEN
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bb := hostConfig(t, cfg, "bitbucket.org")
	if bb.SecretKey != "BB_TOKEN" {
		t.Errorf("bitbucket.org SecretKey = %q, want %q (legacy hosts value must win over built-in default \"bitbucket-token\")",
			bb.SecretKey, "BB_TOKEN")
	}
	if bb.Forge != gitgateway.ForgeBitbucket {
		t.Errorf("bitbucket.org Forge = %q, want %q", bb.Forge, gitgateway.ForgeBitbucket)
	}
}

// Built-in id "github" has a fixed host (github.com). Writing a different
// host under it is almost certainly a mistake — it would silently break
// Basic-auth username selection — so it must be rejected up front, not
// silently accepted. Same for "bitbucket".
func TestLoadFromPath_GatewayForges_BuiltinHostOverrideRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github:
      host: typo.example.com
      secret_key: gh-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for host override on built-in id \"github\", got nil")
	}
}

func TestLoadFromPath_GatewayForges_BuiltinForgeOverrideRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github:
      forge: bitbucket
      secret_key: gh-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for forge override on built-in id \"github\", got nil")
	}
}

// The rejection is against changing a built-in id's host / forge. Writing
// them redundantly with values that already match the built-in defaults
// (e.g. `github: {host: github.com}`) must still be accepted — otherwise
// migrating from `hosts:` by literally moving entries under `forges:` would
// spuriously fail.
func TestLoadFromPath_GatewayForges_BuiltinRedundantHostAllowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  forges:
    github:
      host: github.com
      forge: github
      secret_key: gh-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gh := hostConfig(t, cfg, "github.com")
	if gh.SecretKey != "gh-pat" {
		t.Errorf("github.com SecretKey = %q, want %q", gh.SecretKey, "gh-pat")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.GC.Enabled {
		t.Error("default GC.Enabled should be true")
	}
	if cfg.GC.Interval != 24*time.Hour {
		t.Errorf("default GC.Interval: got %v, want 24h", cfg.GC.Interval)
	}
	if cfg.GC.OlderThan != 720*time.Hour {
		t.Errorf("default GC.OlderThan: got %v, want 720h", cfg.GC.OlderThan)
	}
	if cfg.Sandbox.Backend != SandboxBackendUserns {
		t.Errorf("default Sandbox.Backend: got %q, want %q (safe default — docs/plans/phase6-container-backend.md §PR7)", cfg.Sandbox.Backend, SandboxBackendUserns)
	}
}

// TestLoadFromPath_SandboxBackend_UnsetDefaultsToUserns pins that a
// config.yaml written before sandbox.backend existed (or one that simply
// omits the key) keeps resolving to the userns backend — the byte-for-byte
// "every existing deployment is unaffected" guarantee docs/plans/
// phase6-container-backend.md §PR7 requires of this cutover.
func TestLoadFromPath_SandboxBackend_UnsetDefaultsToUserns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sandbox.Backend != SandboxBackendUserns {
		t.Errorf("Sandbox.Backend = %q, want %q", cfg.Sandbox.Backend, SandboxBackendUserns)
	}
}

// TestLoadFromPath_SandboxBackend_Container pins the opt-in path: an
// explicit `sandbox.backend: container` in config.yaml round-trips to
// SandboxBackendContainer.
func TestLoadFromPath_SandboxBackend_Container(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sandbox:\n  backend: container\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sandbox.Backend != SandboxBackendContainer {
		t.Errorf("Sandbox.Backend = %q, want %q", cfg.Sandbox.Backend, SandboxBackendContainer)
	}
}

// TestLoadFromPath_SandboxBackend_UnrecognizedRejected pins that a typo
// (e.g. "docker" instead of "container") is a hard load error, not a silent
// fallback in either direction — see SandboxConfig.Backend's doc comment.
func TestLoadFromPath_SandboxBackend_UnrecognizedRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sandbox:\n  backend: docker\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected an error for an unrecognized sandbox.backend value, got nil")
	}
}

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
	Log     LogConfig     `yaml:"log"`
	Web     WebConfig     `yaml:"web"`
	Notify  NotifyConfig  `yaml:"notify"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	TaskAsk TaskAskConfig `yaml:"task_ask"`
	Gateway GatewayConfig `yaml:"gateway"`
	// Services declares the API gateway's service registry (docs/plans/
	// api-gateway.md §2) — a top-level key, deliberately NOT nested under
	// Gateway (which is git-gateway-specific: per-forge Basic-auth
	// credential config). See ServiceConfig's own doc comment.
	Services map[string]ServiceConfig `yaml:"services,omitempty"`
	// ServicesFloor is the daemon-wide set of service names enabled for
	// EVERY workspace, mirroring sandbox.allowed_domains' own floor role
	// (docs/plans/api-gateway.md §3: "allowed_domains の floor + workspace
	// 加算方式を鏡写しにする"). A workspace's own
	// WorkspaceMeta.Services list adds to this floor — see
	// orchestrator.ResolveEnabledServices. An entry naming a service not
	// declared under Services is not a load error (see UnmarshalYAML's
	// handling below) — it just never resolves to anything at dispatch
	// time, the same lenient-string-list posture allowed_domains' floor has
	// always had.
	ServicesFloor []string `yaml:"services_floor,omitempty"`
	// OAuthProviders declares the API gateway's OAuth2 provider registry
	// (docs/plans/api-gateway.md §6/§論点4, PR2: "config.yaml の
	// oauth_providers: ブロック。client_secret のみ SecretStore 参照"), keyed
	// by provider name — the same name a services.<name>.auth.provider
	// entry references. A provider reference naming an entry not declared
	// here is not a config-load error — see
	// TestLoadFromPath_Services_OAuth2ProviderReferenceNotCrossValidated's
	// doc comment (oauth_providers_test.go) for why: it fails at request
	// time instead, with a clear error, the same "unresolvable reference
	// surfaces at request time, not load time" contract every other
	// secret-store-shaped reference in this file already has.
	OAuthProviders map[string]OAuthProviderConfig `yaml:"oauth_providers,omitempty"`
	// Integrations configures where the daemon looks for installed
	// Integration Packs (docs/plans/signal-ingest-detailed-design.md §6.1,
	// docs/plans/signal-driven-review.md §6). A top-level key, deliberately
	// separate from Services: the Pack registry is loaded/validated by
	// internal/integrationpack (not this package — see IntegrationsConfig's
	// own doc comment for why), while services.*.uses just references it.
	Integrations IntegrationsConfig `yaml:"integrations,omitempty"`
}

// IntegrationsConfig configures the Integration Pack loader
// (internal/integrationpack.LoadPacks, docs/plans/signal-ingest-detailed-design.md
// §6.1). Deliberately just a directory path — the actual pack enumeration/
// parsing/validation is internal/integrationpack's job, not this package's:
// Config.UnmarshalYAML only ever does SYNTACTIC validation (docs/plans/
// signal-ingest-detailed-design.md §6.2: "config.Load() は構文検証のみ
// (manifest の IO を config parse に持ち込まない)") — resolving this path
// against the filesystem happens once, eagerly, at daemon startup, not on
// every config.yaml parse (a CLI invocation like `boid config get` must not
// need filesystem/Pack-registry access just to read a scalar).
type IntegrationsConfig struct {
	// Dir is the filesystem root internal/integrationpack.LoadPacks
	// enumerates: <dir>/<pack>/<version>/integration.yaml. Defaults to
	// "/opt/boid/integrations" — the compose volume mount point a
	// container-backend deployment bind-mounts an Integration Pack repo
	// checkout onto (docs/plans/signal-driven-review.md §6.4's "配布の v0");
	// a bare binary deployment overrides this to wherever its own Pack
	// checkout lives. A directory that does not exist (the common case for
	// a deployment with no Packs installed yet) is not an error — see
	// LoadPacks' own doc comment.
	Dir string `yaml:"dir,omitempty"`
}

// LogConfig holds daemon logging settings.
//
// Level controls slog's built-in bridge-to-log-package minimum level
// (slog.SetLogLoggerLevel — applied once at daemon startup by
// internal/daemon.ApplyLogLevel, called from cmd/start.go's runDaemonChild)
// WITHOUT installing a custom slog.Handler and WITHOUT changing boid.log's
// line format at all. Nothing in the daemon calls slog.SetDefault or
// installs a Handler (grep the whole tree — internal/dispatcher/
// container_backend_workspace_init.go's own doc comment notes this exact
// fact), so every slog.Info/Debug/Warn/Error call already goes through
// slog's package-private defaultHandler, which formats via the standard
// "log" package (today's "2009/11/10 23:00:00 INFO msg key=value" lines) and
// checks slog.SetLogLoggerLevel's threshold before emitting anything.
// Calling that one function is therefore sufficient to gate slog.Debug
// output on/off — it changes nothing about HOW a line is formatted, only
// WHETHER a given level's line is emitted at all. A real TextHandler/
// JSONHandler would instead produce "time=... level=... msg=..." lines,
// breaking every existing boid.log-grepping runbook (daemon liveness
// checks, etc.) — which is exactly why this package never installs one.
//
// Empty (the zero value, and every config.yaml written before this field
// existed) means "leave slog's own built-in default (info) alone" — the
// exact pre-this-field behavior, byte-for-byte.
type LogConfig struct {
	// Level is one of LogLevelNames ("debug"/"info"/"warn"/"error").
	// Config.UnmarshalYAML rejects any other non-empty value as a hard
	// config-load error (ParseLogLevel), the same treatment
	// gateway.forges.*.forge's unrecognized-forge case and gc.interval's
	// invalid-duration case already get elsewhere in this file — a typo'd
	// level should fail `boid start`/`boid config apply` loudly, not
	// silently fall back to some default.
	Level string `yaml:"level,omitempty"`
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
//
// Backend (formerly a SandboxBackendKind field selecting "userns" vs
// "container") was removed in PR-4 (docs/plans/volume-only-daemon.md
// §論点e): container is now the only sandbox backend, so there is nothing
// left to select. An old config.yaml that still sets sandbox.backend keeps
// loading without error — see UnmarshalYAML's handling below — the key is
// just parsed-and-ignored, with a warning logged.
type SandboxConfig struct {
	AllowedDomains []string `yaml:"allowed_domains"`
	// EgressProxyPortLow/High bound the port band the egress proxy
	// allocates each workspace's stable listener port from, inclusive
	// (docs/plans/egress-proxy-stable-port.md).
	//
	// Both zero (the default, and every config.yaml written before these
	// keys existed) means "use internal/sandbox's own default band", which
	// sits below the kernel's ephemeral port range on purpose — see
	// sandbox.DefaultProxyPortRangeLow's doc comment. Override only when
	// that assumption does not hold locally, e.g. an operator who has
	// lowered net.ipv4.ip_local_port_range so the default band overlaps it.
	EgressProxyPortLow  int `yaml:"egress_proxy_port_low,omitempty"`
	EgressProxyPortHigh int `yaml:"egress_proxy_port_high,omitempty"`
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
//
// For built-in ids ("github" / "bitbucket"), the host and forge fields are
// fixed — they identify what the id *means* — so only secret_key may be
// overridden by the user. Writing `forges: {github: {host: "typo.example.com"}}`
// is almost certainly a mistake (silently pointing the "github" slot at some
// unrelated host would break Basic-auth username selection), so this rejects
// any explicit host/forge value on a built-in id that doesn't match its
// defaults, instead of silently letting it through. Custom ids must supply
// host / forge / secret_key themselves (no defaults apply).
func resolveForgeConfig(id string, fc ForgeConfig) (gitgateway.HostForgeConfig, error) {
	h := gitgateway.HostForgeConfig{Host: fc.Host, Forge: fc.Forge, SecretKey: fc.SecretKey}
	if def, ok := builtinForges[id]; ok {
		if fc.Host != "" && fc.Host != def.Host {
			return gitgateway.HostForgeConfig{}, fmt.Errorf(
				"gateway.forges[%q]: built-in id has fixed host %q; drop the \"host\" field (only \"secret_key\" is overridable on built-in ids). "+
					"To point at a different host, add a custom forge id instead (e.g. \"github-enterprise\").",
				id, def.Host)
		}
		if fc.Forge != "" && fc.Forge != def.Forge {
			return gitgateway.HostForgeConfig{}, fmt.Errorf(
				"gateway.forges[%q]: built-in id has fixed forge %q; drop the \"forge\" field (only \"secret_key\" is overridable on built-in ids)",
				id, def.Forge)
		}
		h.Host = def.Host
		h.Forge = def.Forge
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
// id-sorted order for determinism.
//
// INVARIANT: callers must only invoke this on a GatewayConfig that has
// already been validated by Config.UnmarshalYAML — that is the sole place
// gateway.forges entries are validated, so on a validated config every
// entry resolves successfully. A resolution failure here is defensive-only
// (a hand-built GatewayConfig that skipped validation) and is skipped
// silently rather than surfaced as an error, since HostConfigs has no error
// return and never should have one under the invariant.
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

// DefaultAllowedDomains returns boid's built-in sandbox.allowed_domains
// floor — the domains every daemon allows regardless of what config.yaml
// says (common AI-agent/package-registry endpoints). Exported (moved here
// from cmd/start.go, which now delegates to it) so internal/server's
// config-hot-reload path (config_edit.go's applyDynamicConfigLocked) can
// recompute "floor ∪ user list" on every sandbox.allowed_domains change
// without internal/server needing to import cmd (which would cycle) or
// duplicate this literal (BLOCKER 2 sibling fix, codex review round 1: the
// pre-fix hot-reload replaced the whole effective list with the sparse
// YAML-only entries, silently dropping every built-in domain the very next
// time an operator touched sandbox.allowed_domains at all).
//
// A fresh copy is returned on every call — callers may freely mutate/append
// to the result.
func DefaultAllowedDomains() []string {
	return []string{
		// AI agents
		".anthropic.com",
		".claude.ai",
		".claude.com",
		"api.openai.com",
		"auth.openai.com",
		"chatgpt.com",
		".models.dev", // opencode model metadata registry
		// Go
		"proxy.golang.org",
		"sum.golang.org",
		// Node
		"registry.npmjs.org",
		// The toolchain itself, not just the package registry: volta (and
		// nvm) fetch a version-pinned node tarball from here, so a repo
		// pinning a node version the runner image does not carry cannot
		// run node/npm/pnpm at all without it (2026-08-11 dogfood).
		"nodejs.org",
		// .NET
		"api.nuget.org",
		// Python
		"pypi.org",
		"files.pythonhosted.org",
		// Docker
		".docker.io",
		"auth.docker.io",
	}
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
		Integrations: IntegrationsConfig{
			Dir: "/opt/boid/integrations",
		},
	}
}

// DefaultPath resolves the default XDG config.yaml path
// (~/.config/boid/config.yaml) without reading it. Exported so callers that
// need the *path* itself — not just its parsed content — have one place to
// ask (internal/server's daemon-side config-editing wiring, docs/plans/
// volume-only-daemon.md §論点 f: `boid config get/set/unset/apply/edit`
// reads-modifies-writes this exact file) instead of re-deriving
// os.UserConfigDir()+"boid/config.yaml" independently and risking drift
// from what Load() itself actually reads.
func DefaultPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "boid", "config.yaml"), nil
}

// Load reads the configuration from the default XDG path (~/.config/boid/config.yaml).
// If the file does not exist, the default configuration is returned without error.
func Load() (*Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return DefaultConfig(), nil
	}
	return loadFromPath(path)
}

// LoadFromPath is loadFromPath's exported counterpart (PR834 PR-2b round-3
// codex review Major 2, docs/plans/volume-only-daemon.md §論点f): a pure
// filesystem read + yaml.v3 decode with no daemon/HTTP dependency at all —
// unlike `boid config get` (cmd/config.go), which always talks to a live
// daemon's HTTP API and therefore has no bootstrap-before-first-boot path.
// Exported so any standalone tooling can resolve a config.yaml's EFFECTIVE
// values — including the strict validation UnmarshalYAML performs — against
// an arbitrary path, not just the CLI process's own XDG default. Same
// not-found semantics as Load()/loadFromPath: a missing file returns
// DefaultConfig(), not an error.
//
// Formerly also the primitive behind `boid config effective-backend`
// (scripts/deploy-container.sh's pre-PR-4 config-seed validation step) —
// that subcommand was removed in PR-4 (docs/plans/volume-only-daemon.md
// §論点e) once "container" became the only sandbox backend, so there was no
// longer an effective backend selection worth validating. LoadFromPath
// itself stays exported as a general-purpose primitive.
func LoadFromPath(path string) (*Config, error) {
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
	// The egress proxy port band is checked HERE, not only in ValidateYAML,
	// because ValidateYAML runs on the `boid config set/edit/apply` paths
	// while this is the path `boid start` itself takes. A hand-edited or
	// deploy-seeded config.yaml would otherwise reach the runtime
	// unvalidated, where a half-set or malformed band degrades silently
	// back to ephemeral ports — the exact "no sign that config.yaml caused
	// it" failure the band exists to prevent
	// (docs/plans/egress-proxy-stable-port.md). Deliberately scoped to this
	// one key rather than turning loadFromPath into a general validator:
	// widening that would change the failure behaviour of every existing
	// config surface at once, which is a separate decision.
	if err := validateEgressProxyPortRange(cfg.Sandbox.EgressProxyPortLow, cfg.Sandbox.EgressProxyPortHigh); err != nil {
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
		Log struct {
			Level string `yaml:"level"`
		} `yaml:"log"`
		Web struct {
			PublicURL string `yaml:"public_url"`
			HTTPAddr  string `yaml:"http_addr"`
		} `yaml:"web"`
		Notify struct {
			Command []string `yaml:"command"`
		} `yaml:"notify"`
		Sandbox struct {
			AllowedDomains      []string `yaml:"allowed_domains"`
			EgressProxyPortLow  int      `yaml:"egress_proxy_port_low"`
			EgressProxyPortHigh int      `yaml:"egress_proxy_port_high"`
			Backend             string   `yaml:"backend"`
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
		Services       map[string]ServiceConfig       `yaml:"services"`
		ServicesFloor  []string                       `yaml:"services_floor"`
		OAuthProviders map[string]OAuthProviderConfig `yaml:"oauth_providers"`
		Integrations   struct {
			Dir string `yaml:"dir"`
		} `yaml:"integrations"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	c.GC = defaults.GC
	c.Log = defaults.Log
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

	if raw.Log.Level != "" {
		if _, err := ParseLogLevel(raw.Log.Level); err != nil {
			return fmt.Errorf("log.level: %w", err)
		}
		c.Log.Level = raw.Log.Level
	}

	c.Web.PublicURL = raw.Web.PublicURL
	c.Web.HTTPAddr = raw.Web.HTTPAddr

	c.Notify.Command = raw.Notify.Command

	c.Sandbox.AllowedDomains = raw.Sandbox.AllowedDomains
	c.Sandbox.EgressProxyPortLow = raw.Sandbox.EgressProxyPortLow
	c.Sandbox.EgressProxyPortHigh = raw.Sandbox.EgressProxyPortHigh

	// sandbox.backend: removed in PR-4 (docs/plans/volume-only-daemon.md
	// §論点e) — container is the only sandbox backend now, so the key
	// carries no meaning any more. Accepted-but-ignored (any value,
	// including a typo/garbage one) rather than a hard load error: an
	// operator's existing config.yaml from before this cutover must keep
	// loading unattended (smooth migration, PR-1b's own KindOpaque
	// precedent for a retired field — see gateway.hosts's fold-in above
	// for the analogous case). A warning is still logged so the key's
	// no-op-ness is discoverable, matching schema.go's KindOpaque
	// rejection message `boid config set/unset sandbox.backend` itself
	// gives for the CLI-editing path.
	if raw.Sandbox.Backend != "" {
		slog.Warn("sandbox.backend is no longer used; container is the only sandbox backend now (docs/plans/volume-only-daemon.md §論点e) — this key is ignored",
			"value", raw.Sandbox.Backend)
	}

	if raw.TaskAsk.DisconnectGrace != "" {
		d, err := time.ParseDuration(raw.TaskAsk.DisconnectGrace)
		if err != nil {
			return fmt.Errorf("task_ask.disconnect_grace: %w", err)
		}
		c.TaskAsk.DisconnectGrace = d
	}

	// Start from the built-in defaults (github/bitbucket), then let the
	// user's gateway.forges entries override/extend them by id. userForges
	// tracks which ids were user-explicit (vs. seeded from defaults), which
	// the legacy gateway.hosts loop below uses to decide whether a
	// legacy-hosts entry for a built-in host should merge into the built-in
	// slot (preserving its secret_key) or be dropped as a duplicate.
	forges := make(map[string]ForgeConfig, len(defaults.Gateway.Forges)+len(raw.Gateway.Forges))
	for id, fc := range defaults.Gateway.Forges {
		forges[id] = fc
	}
	userForges := make(map[string]struct{}, len(raw.Gateway.Forges))
	for id, fc := range raw.Gateway.Forges {
		forges[id] = fc
		userForges[id] = struct{}{}
	}

	// Resolve gateway.forges first (fully validating it, including built-in
	// host/forge override rejection). Build host -> forge id lookup so the
	// legacy gateway.hosts loop can apply the right priority rule per host:
	// forges wins for user-explicit entries; but a legacy hosts entry that
	// matches a *built-in default host* which the user hasn't explicitly
	// configured via forges must MERGE into the built-in slot, not be
	// dropped — otherwise the legacy entry's non-default secret_key gets
	// silently lost to the built-in "github-pat" / "bitbucket-token"
	// default (the regression this fix targets).
	resolvedHosts := make(map[string]string, len(forges))
	for id, fc := range forges {
		h, err := resolveForgeConfig(id, fc)
		if err != nil {
			return err
		}
		resolvedHosts[h.Host] = id
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
			if existingID, dup := resolvedHosts[h.Host]; dup {
				_, userExplicit := userForges[existingID]
				_, isBuiltin := builtinForges[existingID]
				if isBuiltin && !userExplicit {
					// Legacy entry targets a built-in host that the user
					// hasn't explicitly configured via gateway.forges.
					// Merge it into the built-in slot so the legacy
					// secret_key wins over the built-in default. This is
					// the "byte-for-byte legacy compat" path — nose's
					// actual config.yaml (hosts-only, GH_TOKEN /
					// BB_TOKEN keys) must keep working for one release.
					slog.Warn("gateway.hosts entry merged into built-in forge slot (legacy secret_key preserved over built-in default)",
						"host", h.Host, "forge_id", existingID)
					forges[existingID] = ForgeConfig{Host: h.Host, Forge: h.Forge, SecretKey: h.SecretKey}
					continue
				}
				slog.Warn("gateway.hosts entry ignored: host is already configured via gateway.forges", "host", h.Host)
				continue
			}
			// Use the host itself as the synthetic forges id: legacy
			// entries already carry host/forge/secret_key fully resolved,
			// so no built-in defaulting ever applies to them.
			forges[h.Host] = ForgeConfig{Host: h.Host, Forge: h.Forge, SecretKey: h.SecretKey}
			resolvedHosts[h.Host] = h.Host
		}
	}
	c.Gateway = GatewayConfig{Forges: forges}

	// services: (docs/plans/api-gateway.md §2). Every entry is validated
	// eagerly here — the same "fail config.yaml load, not just skip the
	// service" posture gateway.forges entries get — since
	// APIGatewayServices' own "already validated" invariant depends on it.
	if len(raw.Services) > 0 {
		for name, sc := range raw.Services {
			if err := validateServiceConfig(name, sc); err != nil {
				return err
			}
		}
		c.Services = raw.Services
	}

	// services_floor: (docs/plans/api-gateway.md §3). Deliberately NOT
	// validated against c.Services the way gateway.forges entries are
	// validated against known forge kinds — mirrors sandbox.
	// allowed_domains' floor, which has never validated its entries against
	// anything either. An entry naming an undeclared service is warned
	// about (a likely typo) but does not fail config load: the floor is
	// still a plain string list at this layer, and dispatch-time resolution
	// (orchestrator.ResolveEnabledServices) simply produces a service name
	// CredentialProvider.KnowsService will 502 on, same as any other
	// unconfigured-service request.
	c.ServicesFloor = raw.ServicesFloor
	for _, name := range raw.ServicesFloor {
		if _, ok := c.Services[name]; !ok {
			slog.Warn("services_floor names a service that is not declared under services:; it will never resolve to anything at dispatch time",
				"service", name)
		}
	}

	// oauth_providers: (docs/plans/api-gateway.md §6/§論点4, PR2). Same
	// eager, fail-config-load posture as services: above — every entry is
	// validated here since APIGatewayOAuthProviders' own "already
	// validated" invariant depends on it.
	if len(raw.OAuthProviders) > 0 {
		for name, pc := range raw.OAuthProviders {
			if err := validateOAuthProviderConfig(name, pc); err != nil {
				return err
			}
		}
		c.OAuthProviders = raw.OAuthProviders
	}

	// integrations.dir (docs/plans/signal-ingest-detailed-design.md §6.1):
	// starts from the default ("/opt/boid/integrations") like every other
	// leaf with a non-empty built-in default (e.g. gateway.forges above) —
	// an omitted or empty integrations.dir must not silently overwrite the
	// default with a zero value.
	c.Integrations = defaults.Integrations
	if raw.Integrations.Dir != "" {
		c.Integrations.Dir = raw.Integrations.Dir
	}

	return nil
}

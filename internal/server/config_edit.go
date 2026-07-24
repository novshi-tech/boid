package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/config"
	"gopkg.in/yaml.v3"
)

// This file implements api.ConfigService's ConfigYAML/ApplyConfigYAML on
// *Server — the daemon-side half of `boid config get/set/unset/apply/edit`
// (docs/plans/volume-only-daemon.md §論点 f). buildRuntime (wire.go)
// populates liveConfig/configPath/notifySvc once at startup; everything
// here is reached only through the GET/POST /api/config routes
// mountRoutes wires to api.ConfigHandler.

// ConfigYAML returns the daemon's config.yaml document exactly as it sits
// on disk, alongside its current revision — see api.ConfigService.ConfigYAML's
// doc comment. The revision is the same ETag value GET /api/config's
// response header carries; round-tripping it into a later POST's If-Match
// (`boid config edit`/`apply -f` without --force) is BLOCKER 1's
// optimistic-concurrency guard (codex review round 1).
//
// Deliberately NOT a fully-expanded (defaults-merged) view of s.liveConfig:
// several Config sub-structs (WebConfig, NotifyConfig, SandboxConfig, ...)
// have no `omitempty` on their yaml tags, so marshaling the merged struct
// would make EVERY key "present" (e.g. `web: {public_url: ""}` even when
// nothing was ever configured) — collapsing `boid config unset`'s "key not
// found" into meaninglessness (a zero-value key would always exist to
// "unset"). Returning the raw file instead keeps get/set/unset/apply/edit
// a faithful, sparse round trip of exactly what the operator has
// explicitly written — the same contract config.yaml has always had
// (docs/ja/reference/config-yaml.md: "ファイルが存在しない場合はデフォルト値で
// 動作します"; per-key defaults are documented in prose there, not by
// requiring the file to spell every one out). A missing file (fresh
// install, nothing explicitly configured yet) returns an empty document,
// not an error.
func (s *Server) ConfigYAML() (data []byte, revision string, err error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	data, err = s.readConfigLocked()
	if err != nil {
		return nil, "", err
	}
	return data, s.revisionLocked(), nil
}

// readConfigLocked reads config.yaml off disk verbatim. Callers must
// already hold configMu — factored out of ConfigYAML so MutateConfig (which
// also needs the current on-disk document, as part of its own single
// configMu-held critical section) doesn't re-acquire the lock it's already
// holding.
func (s *Server) readConfigLocked() ([]byte, error) {
	if s.configPath == "" {
		return nil, fmt.Errorf("daemon config path not configured")
	}
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	return data, nil
}

// revisionLocked renders the current configRevision counter as the string
// ETag/If-Match value. Callers must already hold configMu.
func (s *Server) revisionLocked() string {
	return strconv.FormatUint(s.configRevision, 10)
}

// ApplyConfigYAML validates, persists, and (for hot-reloadable keys)
// live-applies a full replacement config.yaml document — see
// api.ConfigService.ApplyConfigYAML's doc comment for the contract.
//
// ifMatch/force implement the same ETag/If-Match convention `boid workspace
// edit` already established (internal/api/project_service.go's
// UpdateWorkspace, cmd/workspace.go's runWorkspaceEdit) — BLOCKER 1, codex
// review round 1: unless force is true, ifMatch must be non-empty (else 428
// Precondition Required) and must match the current revision (else 412
// Precondition Failed). This is what makes `boid config edit`/`apply -f`
// (without --force) reject a write against config.yaml that changed since
// the caller's GET, instead of silently discarding whatever the other
// writer just applied.
//
// The whole check-validate-diff-write-swap sequence runs under configMu, so
// concurrent POST /api/config calls are strictly serialized (docs/plans/
// volume-only-daemon.md §論点 f's concurrency decision) — the second call's
// revision check, validation, and diff always see the first call's
// already-applied liveConfig/configRevision, never a half-applied
// intermediate state, and the on-disk config.yaml is always written by
// WriteFileAtomic's temp+rename (never a byte-interleaved torn file).
func (s *Server) ApplyConfigYAML(data []byte, ifMatch string, force bool) (api.ConfigApplyResult, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	if !force {
		if ifMatch == "" {
			return api.ConfigApplyResult{}, &api.StatusError{
				Code:    http.StatusPreconditionRequired,
				Message: "If-Match header is required (or pass --force / ?force=true)",
			}
		}
		if ifMatch != s.revisionLocked() {
			return api.ConfigApplyResult{}, &api.StatusError{
				Code:    http.StatusPreconditionFailed,
				Message: fmt.Sprintf("revision mismatch: If-Match %q does not match the current revision %q", ifMatch, s.revisionLocked()),
			}
		}
	}

	return s.applyConfigYAMLLocked(data)
}

// applyConfigYAMLLocked runs the actual validate-write-swap-hot-apply
// sequence, bumping configRevision on every successful write. Callers
// (ApplyConfigYAML, MutateConfig) must already hold configMu — factored out
// so MutateConfig's server-side read-modify-write can reuse this exact
// logic after building its own replacement document, without any If-Match
// check of its own (MutateConfig never has a client-supplied stale
// snapshot to guard against in the first place — see its own doc comment).
func (s *Server) applyConfigYAMLLocked(data []byte) (api.ConfigApplyResult, error) {
	newCfg, err := config.ValidateYAML(data)
	if err != nil {
		return api.ConfigApplyResult{}, err
	}

	if s.configPath == "" {
		return api.ConfigApplyResult{}, fmt.Errorf("daemon config path not configured")
	}
	if err := os.MkdirAll(filepath.Dir(s.configPath), 0o755); err != nil {
		return api.ConfigApplyResult{}, fmt.Errorf("create config dir: %w", err)
	}
	if err := config.WriteFileAtomic(s.configPath, data, 0o600); err != nil {
		return api.ConfigApplyResult{}, fmt.Errorf("write config: %w", err)
	}

	oldCfg := s.liveConfig
	s.liveConfig = newCfg
	s.configRevision++

	warnings := s.applyDynamicConfigLocked(oldCfg, newCfg)
	return api.ConfigApplyResult{Warnings: warnings}, nil
}

// MutateConfig performs a single dotted-path set/unset as one
// configMu-held read-modify-write — BLOCKER 1, codex review round 1: `boid
// config set/unset` (cmd/config.go) route here instead of the old
// client-side GET → mutate → POST round trip, whose two separate daemon
// calls left a window for a second concurrent `set`'s POST to silently
// discard a first `set`'s already-applied change (configMu only serialized
// the POSTs, not the whole client-side transaction around them). Holding
// configMu across the read, the dotted-path Set/Unset, and the write
// closes that window entirely: two concurrent MutateConfig calls on
// different keys are strictly serialized, and BOTH changes land — see
// TestMutateConfig_ConcurrentSetsOfDifferentKeys_BothSucceed.
//
// No If-Match/force parameter, unlike ApplyConfigYAML: there is no
// client-supplied stale snapshot to protect against here at all, since the
// caller only sends an operation + key + value, never a full document — the
// snapshot MutateConfig reads is always the current one, read fresh under
// the very same lock that performs the write.
func (s *Server) MutateConfig(req api.ConfigMutateRequest) (api.ConfigMutateResult, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	data, err := s.readConfigLocked()
	if err != nil {
		return api.ConfigMutateResult{}, err
	}
	tree, err := config.ParseTree(data)
	if err != nil {
		return api.ConfigMutateResult{}, err
	}

	switch req.Op {
	case api.ConfigMutateSet:
		if _, err := config.Set(tree, req.Key, req.Value); err != nil {
			return api.ConfigMutateResult{}, err
		}
	case api.ConfigMutateUnset:
		if _, err := config.Unset(tree, req.Key); err != nil {
			return api.ConfigMutateResult{}, err
		}
	default:
		return api.ConfigMutateResult{}, fmt.Errorf("unknown op %q (want %q or %q)", req.Op, api.ConfigMutateSet, api.ConfigMutateUnset)
	}

	newData, err := yaml.Marshal(tree)
	if err != nil {
		return api.ConfigMutateResult{}, fmt.Errorf("marshal config: %w", err)
	}

	result, err := s.applyConfigYAMLLocked(newData)
	if err != nil {
		return api.ConfigMutateResult{}, err
	}
	return api.ConfigMutateResult{
		ConfigApplyResult: result,
		YAML:              newData,
		Revision:          s.revisionLocked(),
	}, nil
}

// applyDynamicConfigLocked hot-applies whatever changed leaf paths the
// daemon can apply without a restart, and returns operator-facing warning
// lines for the ones it cannot. Called with configMu already held.
//
// oldCfg is nil the very first time ApplyConfigYAML runs before
// buildRuntime ever populated liveConfig (should not happen on any live
// route — mountRoutes only registers GET/POST /api/config after
// buildRuntime returns — but handled defensively rather than assumed).
func (s *Server) applyDynamicConfigLocked(oldCfg, newCfg *config.Config) []string {
	var oldAllowed, oldCommand []string
	var oldPublicURL string
	var oldForges map[string]config.ForgeConfig
	var oldBackend config.SandboxBackendKind
	if oldCfg != nil {
		oldAllowed = oldCfg.Sandbox.AllowedDomains
		oldCommand = oldCfg.Notify.Command
		oldPublicURL = oldCfg.Web.PublicURL
		oldForges = oldCfg.Gateway.Forges
		oldBackend = oldCfg.Sandbox.Backend
	}
	// oldCfgSafe backs the generic restart-required-field loop below —
	// never nil, so restartFieldExtractors' funcs can dereference oldCfg's
	// sub-structs unconditionally. Mirrors the same "oldCfg nil only on the
	// defensive first-ever-call path" contract the individual oldX vars
	// above already follow.
	oldCfgSafe := oldCfg
	if oldCfgSafe == nil {
		oldCfgSafe = &config.Config{}
	}

	// sandbox.allowed_domains — dynamic: s.cfg.AllowedDomains is read fresh,
	// under s.mu, by (*Server).AllowedDomains — the exact method value
	// wire.go wires as dispatcher.Runner.AllowedDomains's getter (BLOCKER 2,
	// codex review round 1) — so swapping it here is a genuine,
	// immediately-effective hot reload reaching the already-constructed
	// Runner, not just this Server's own config struct. The effective value
	// is ALWAYS recomputed as the built-in floor UNION the just-applied
	// user list (config.DefaultAllowedDomains() ∪
	// newCfg.Sandbox.AllowedDomains), never newCfg's sparse list alone —
	// the pre-fix assignment here silently dropped every built-in domain
	// (anthropic.com, npm registry, ...) the moment an operator touched
	// sandbox.allowed_domains at all.
	if !stringSlicesEqual(oldAllowed, newCfg.Sandbox.AllowedDomains) {
		s.mu.Lock()
		s.cfg.AllowedDomains = mergeAllowedDomains(config.DefaultAllowedDomains(), newCfg.Sandbox.AllowedDomains)
		s.mu.Unlock()
		slog.Info("config: hot-reloaded", "key", "sandbox.allowed_domains", "count", len(newCfg.Sandbox.AllowedDomains))
	}

	// notify.command / web.public_url — dynamic: hot-applied via
	// notify.Service.Update on the very instance wire.go shares with the
	// git gateway's notifier and TaskAppService.Notify.
	if s.notifySvc != nil && (!stringSlicesEqual(oldCommand, newCfg.Notify.Command) || oldPublicURL != newCfg.Web.PublicURL) {
		s.notifySvc.Update(newCfg.Notify.Command, newCfg.Web.PublicURL)
		slog.Info("config: hot-reloaded", "key", "notify.command,web.public_url")
	}

	var warnings []string

	// gateway.forges.* — restart-required (mid-flight gateway TLS cert
	// re-issuance is complicated; safer to require a restart — docs/plans/
	// volume-only-daemon.md §論点 f). One warning line per changed LEAF
	// (host/forge/secret_key), not per forge id — MINOR 2 (codex review
	// round 1): the plan doc's own example
	// ("gateway.forges.github.secret_key requires...") names the leaf that
	// actually changed, which a forge-id-only warning could not distinguish
	// from an unrelated field on the same entry changing instead.
	for _, leaf := range changedForgeLeaves(oldForges, newCfg.Gateway.Forges) {
		warnings = append(warnings, restartWarning("gateway.forges."+leaf))
	}

	// Every other ReloadRestartRequired schema leaf (gc.*, web.http_addr,
	// task_ask.disconnect_grace, ...) — MAJOR 2, codex review round 1: the
	// pre-fix version of this function hand-listed only gateway.forges.*
	// and sandbox.backend, silently producing no warning at all for every
	// other restart-required key (e.g. `boid config set gc.enabled false`
	// printed a bare "config applied" while the already-running GC loop
	// kept its old interval/enabled state until the next restart). Iterating
	// config.Schema directly means a future schema addition only needs an
	// entry in restartFieldExtractors, not a new arm here.
	// gateway.forges.* is skipped (restartFieldExtractors has no entry for
	// its wildcard path; the per-leaf diff above already covers it with
	// finer granularity than a single wildcard comparison could).
	// gateway.hosts is skipped too: Config retains no Hosts field after
	// UnmarshalYAML folds it into Forges, so any of its changes already
	// surface through the Forges diff above.
	for _, spec := range config.Schema {
		if spec.Reload != config.ReloadRestartRequired {
			continue
		}
		extract, ok := restartFieldExtractors[spec.Path]
		if !ok {
			continue
		}
		if extract(oldCfgSafe) != extract(newCfg) {
			warnings = append(warnings, restartWarning(spec.Path))
		}
	}

	// sandbox.backend — still fully valid (its removal is PR-4, docs/plans/
	// volume-only-daemon.md §論点 e); this PR must not reject writes to it.
	// The retirement notice fires only when this apply actually changed
	// the value — an unrelated `set sandbox.allowed_domains ...` call must
	// not nag about a key nobody touched. Never hot-applied: JobRuntime
	// backend selection (dispatcher.Runner.Backend) is wired once at
	// daemon startup (buildRuntime/sandboxBackendForConfig), so an
	// operator changing this key genuinely does need a restart for
	// dispatch itself to observe it, even though that is not spelled out
	// as its own separate warning here — the retirement notice already
	// tells the operator this key is not a normal live setting.
	if newCfg.Sandbox.Backend != oldBackend {
		warnings = append(warnings, "[warning] sandbox.backend is on the retirement path — see docs/plans/volume-only-daemon.md §論点 e; will be removed in PR-4")
	}

	return warnings
}

// restartWarning renders the standard, plan-doc-exact
// (docs/plans/volume-only-daemon.md §論点 f) restart-required warning line
// for a single changed leaf path (e.g. "gc.enabled",
// "gateway.forges.github.secret_key") — the one piece of text every
// restart-required warning site shares, factored out so MAJOR 2's generic
// loop and the per-forge-leaf loop above stay byte-for-byte consistent.
func restartWarning(path string) string {
	return fmt.Sprintf(
		"[warning] %s requires daemon restart to take effect.\n"+
			"          Restart with: docker compose -f build/container/compose.yml restart daemon", path)
}

// restartFieldExtractors maps each non-wildcard ReloadRestartRequired
// schema leaf (schema.go) this package's generic restart-warning loop
// (applyDynamicConfigLocked) can compare directly on a *config.Config, to a
// string rendering of that leaf's value — equality of the rendering is
// "unchanged", inequality is "changed, warn" (MAJOR 2, codex review round
// 1: "the warning generation logic should iterate over the schema's
// restart-required list" instead of hand-listing which keys it happens to
// check). Adding a new restart-required scalar to Schema needs only a new
// entry here, not a new arm in applyDynamicConfigLocked.
//
// gateway.forges.* is deliberately absent: changedForgeLeaves's per-id,
// per-field diff already gives finer-grained warnings (MINOR 2) than a
// single wildcard schema entry could express, and is iterated separately.
// gateway.hosts is also absent: Config.UnmarshalYAML always folds it into
// Gateway.Forges before this function ever runs, so Config retains no Hosts
// field of its own to compare — any of its changes surface through the
// Forges diff instead.
var restartFieldExtractors = map[string]func(*config.Config) string{
	"gc.enabled":                func(c *config.Config) string { return strconv.FormatBool(c.GC.Enabled) },
	"gc.interval":               func(c *config.Config) string { return c.GC.Interval.String() },
	"gc.older_than":             func(c *config.Config) string { return c.GC.OlderThan.String() },
	"web.http_addr":             func(c *config.Config) string { return c.Web.HTTPAddr },
	"task_ask.disconnect_grace": func(c *config.Config) string { return c.TaskAsk.DisconnectGrace.String() },
}

// mergeAllowedDomains returns floor followed by every entry of user not
// already present in floor, deduplicated (order-preserving) — the
// authoritative "effective sandbox.allowed_domains" computation
// (docs/plans/volume-only-daemon.md §論点 f, BLOCKER 2 sibling fix, codex
// review round 1). floor is boid's built-in default domains
// (config.DefaultAllowedDomains()); user is config.yaml's
// sandbox.allowed_domains as written. Mirrors, and is the sole hot-reload-
// time counterpart of, cmd/start.go's buildStartConfig boot-time
// concatenation — that one only runs once at daemon startup and cannot
// itself react to a later `boid config set sandbox.allowed_domains ...`.
func mergeAllowedDomains(floor, user []string) []string {
	out := make([]string, 0, len(floor)+len(user))
	seen := make(map[string]struct{}, len(floor)+len(user))
	for _, d := range floor {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	for _, d := range user {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// changedForgeLeaves returns the sorted set of dotted "<id>.<leaf>" strings
// (e.g. "github.secret_key") for every gateway.forges leaf that actually
// differs between oldForges and newForges (MINOR 2, codex review round 1:
// name the exact leaf that changed, not just the forge id). A forge id
// added or removed wholesale reports just "<id>" (no single leaf is more
// "the" changed one than another when the whole entry appeared/vanished).
func changedForgeLeaves(oldForges, newForges map[string]config.ForgeConfig) []string {
	ids := make(map[string]struct{}, len(oldForges)+len(newForges))
	for id := range oldForges {
		ids[id] = struct{}{}
	}
	for id := range newForges {
		ids[id] = struct{}{}
	}
	var changed []string
	for id := range ids {
		o, oOK := oldForges[id]
		n, nOK := newForges[id]
		switch {
		case !oOK || !nOK:
			// Whole entry added or removed — no single existing leaf to
			// blame, report the entry itself.
			if oOK != nOK {
				changed = append(changed, id)
			}
		default:
			if o.Host != n.Host {
				changed = append(changed, id+".host")
			}
			if o.Forge != n.Forge {
				changed = append(changed, id+".forge")
			}
			if o.SecretKey != n.SecretKey {
				changed = append(changed, id+".secret_key")
			}
		}
	}
	sort.Strings(changed)
	return changed
}

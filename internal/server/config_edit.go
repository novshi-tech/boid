package server

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/config"
)

// This file implements api.ConfigService's ConfigYAML/ApplyConfigYAML on
// *Server — the daemon-side half of `boid config get/set/unset/apply/edit`
// (docs/plans/volume-only-daemon.md §論点 f). buildRuntime (wire.go)
// populates liveConfig/configPath/notifySvc once at startup; everything
// here is reached only through the GET/POST /api/config routes
// mountRoutes wires to api.ConfigHandler.

// ConfigYAML returns the daemon's config.yaml document exactly as it sits
// on disk — see api.ConfigService.ConfigYAML's doc comment.
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
func (s *Server) ConfigYAML() ([]byte, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
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

// ApplyConfigYAML validates, persists, and (for hot-reloadable keys)
// live-applies a full replacement config.yaml document — see
// api.ConfigService.ApplyConfigYAML's doc comment for the contract.
//
// The whole validate-diff-write-swap sequence runs under configMu, so two
// concurrent POST /api/config calls are strictly serialized (docs/plans/
// volume-only-daemon.md §論点 f's concurrency decision) — the second call's
// validation and diff always see the first call's already-applied
// liveConfig, never a half-applied intermediate state, and the on-disk
// config.yaml is always written by WriteFileAtomic's temp+rename (never a
// byte-interleaved torn file).
func (s *Server) ApplyConfigYAML(data []byte) (api.ConfigApplyResult, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()

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

	warnings := s.applyDynamicConfigLocked(oldCfg, newCfg)
	return api.ConfigApplyResult{Warnings: warnings}, nil
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

	// sandbox.allowed_domains — dynamic: s.cfg.AllowedDomains is read fresh
	// by sandbox.ProxyManager.GetOrCreate on every dispatch (server.go),
	// so swapping it here under s.mu is a genuine, immediately-effective
	// hot reload — no further plumbing needed.
	if !stringSlicesEqual(oldAllowed, newCfg.Sandbox.AllowedDomains) {
		s.mu.Lock()
		s.cfg.AllowedDomains = append([]string(nil), newCfg.Sandbox.AllowedDomains...)
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
	// volume-only-daemon.md §論点 f). One warning line per changed forge
	// id, exact wording from the plan doc.
	for _, id := range changedForgeIDs(oldForges, newCfg.Gateway.Forges) {
		warnings = append(warnings, fmt.Sprintf(
			"[warning] gateway.forges.%s requires daemon restart to take effect.\n"+
				"          Restart with: docker compose -f build/container/compose.yml restart daemon", id))
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

// changedForgeIDs returns the sorted set of forge ids present in old or
// new whose resolved ForgeConfig differs (added, removed, or field
// changed) between the two maps.
func changedForgeIDs(oldForges, newForges map[string]config.ForgeConfig) []string {
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
		if oOK != nOK || !reflect.DeepEqual(o, n) {
			changed = append(changed, id)
		}
	}
	sort.Strings(changed)
	return changed
}

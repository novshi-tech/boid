package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
	return data, computeRevision(data), nil
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

// computeRevision derives a content-addressed ETag/If-Match revision from a
// config.yaml document's raw bytes — BLOCKER (codex review round 2): the
// pre-fix revision was an in-memory, process-lifetime counter
// (Server.configRevision, now removed) that always reset to 1 on daemon
// restart, regardless of what was actually on disk. That let a stale
// If-Match alias a genuinely different (newer) document across a restart:
// editor A's GET captured revision "1"; writer B's apply persisted a change
// and bumped the counter to "2"; the daemon restarted and reset the counter
// back to "1"; a fresh GET after the restart reported B's now-current
// document as revision "1" again; A's later POST with If-Match: "1" matched
// and silently discarded B's change. A revision derived purely from the
// document's own bytes cannot alias this way — it depends only on what is
// on disk, so it is:
//   - stable across process restarts for byte-identical content (no
//     restart-reset counter to disagree with the file),
//   - different whenever content actually differs (any single-byte change
//     flips the hash with overwhelming probability), and
//   - never colliding by accident between two DIFFERENT documents (and a
//     "collision" between two byte-identical documents is not a bug — there
//     is nothing to lose by treating identical content as the same
//     revision).
//
// sha256, truncated to its first 16 bytes (32 hex chars, 128 bits) — MINOR
// (codex review round 3): an earlier version truncated to 8 bytes (64
// bits), whose ~2^-32 accidental-collision risk at 2^32 distinct documents
// is small in practice but not zero, and a collision here recreates
// exactly the stale-ETag overwrite failure this revision scheme exists to
// prevent (see the doc comment above). 128 bits' collision resistance is
// effectively unreachable for a handful-of-KB config.yaml edited by a
// handful of humans, at a trivial (16 extra hex chars) header cost — no
// reason to settle for less.
func computeRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// currentRevisionLocked re-reads config.yaml off disk and returns its
// current content-derived revision — the same value a fresh GET /api/config
// would report right now. Callers must already hold configMu. Deliberately
// re-reads rather than trusting any cached value, so ApplyConfigYAML's
// If-Match check always compares against the true current on-disk state.
func (s *Server) currentRevisionLocked() (string, error) {
	data, err := s.readConfigLocked()
	if err != nil {
		return "", err
	}
	return computeRevision(data), nil
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
// already-applied liveConfig and on-disk bytes (currentRevisionLocked
// re-reads them fresh under this same lock), never a half-applied
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
		currentRev, err := s.currentRevisionLocked()
		if err != nil {
			return api.ConfigApplyResult{}, err
		}
		if ifMatch != currentRev {
			return api.ConfigApplyResult{}, &api.StatusError{
				Code:    http.StatusPreconditionFailed,
				Message: fmt.Sprintf("revision mismatch: If-Match %q does not match the current revision %q", ifMatch, currentRev),
			}
		}
	}

	return s.applyConfigYAMLLocked(data)
}

// applyConfigYAMLLocked runs the actual validate-write-swap-hot-apply
// sequence; the revision the next GET/If-Match check observes simply follows
// from the new on-disk bytes (computeRevision), no counter to bump. Callers
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

	warnings := s.applyDynamicConfigLocked(oldCfg, newCfg)
	return api.ConfigApplyResult{Warnings: warnings}, nil
}

// MutateConfig performs one or more dotted-path set/unset operations as one
// configMu-held read-modify-write — BLOCKER 1, codex review round 1: `boid
// config set/unset` (cmd/config.go) route here instead of the old
// client-side GET → mutate → POST round trip, whose two separate daemon
// calls left a window for a second concurrent `set`'s POST to silently
// discard a first `set`'s already-applied change (configMu only serialized
// the POSTs, not the whole client-side transaction around them). Holding
// configMu across the read, every op's dotted-path Set/Unset, and the write
// closes that window entirely: two concurrent MutateConfig calls on
// different keys are strictly serialized, and BOTH changes land — see
// TestMutateConfig_ConcurrentSetsOfDifferentKeys_BothSucceed.
//
// Batch mode (BLOCKER, codex review round 1 on PR #831): when req.Ops is
// non-empty, every element is applied to the SAME in-memory tree in order,
// and the resulting document is validated exactly ONCE, after the last op —
// not once per op. This is what makes it possible to create a brand-new
// gateway.forges.<id> entry at all: a single-op call always validates the
// full document before returning, so setting "<id>.host" alone leaves the
// document with an empty "<id>.forge", which config.ValidateYAML rejects
// ("unrecognized forge \"\"") — no sequence of independently-validated
// single-op calls can ever get all three of host/forge/secret_key in place
// together. When req.Ops is empty, req itself is treated as the sole
// operation — the exact pre-existing single-op behavior, unchanged.
// See TestMutateConfig_Batch_NewForgeAllFieldsAtOnce_Succeeds and
// TestMutateConfig_Batch_PartialFailureLeavesDocumentUnchanged.
//
// No If-Match/force parameter, unlike ApplyConfigYAML: there is no
// client-supplied stale snapshot to protect against here at all, since the
// caller only sends operations (op + key + value), never a full document —
// the snapshot MutateConfig reads is always the current one, read fresh
// under the very same lock that performs the write.
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

	ops := req.Ops
	if len(ops) == 0 {
		ops = []api.ConfigMutateRequest{req}
	}

	for _, op := range ops {
		switch op.Op {
		case api.ConfigMutateSet:
			if _, err := config.Set(tree, op.Key, op.Value); err != nil {
				return api.ConfigMutateResult{}, err
			}
		case api.ConfigMutateUnset:
			if _, err := config.Unset(tree, op.Key); err != nil {
				return api.ConfigMutateResult{}, err
			}
		default:
			return api.ConfigMutateResult{}, fmt.Errorf("unknown op %q (want %q or %q)", op.Op, api.ConfigMutateSet, api.ConfigMutateUnset)
		}
	}

	newData, err := yaml.Marshal(tree)
	if err != nil {
		return api.ConfigMutateResult{}, fmt.Errorf("marshal config: %w", err)
	}

	// applyConfigYAMLLocked is the single validation point for the whole
	// batch (config.ValidateYAML runs once here, against the tree with
	// every op already applied) — never called per-op above, which is
	// exactly what lets a multi-leaf structural change succeed. A
	// validation failure at this point means NONE of tree's in-memory
	// mutations were ever written to disk (applyConfigYAMLLocked validates
	// before it writes), so a bad op anywhere in the batch leaves
	// config.yaml byte-for-byte as it was before this call.
	result, err := s.applyConfigYAMLLocked(newData)
	if err != nil {
		return api.ConfigMutateResult{}, err
	}
	return api.ConfigMutateResult{
		ConfigApplyResult: result,
		YAML:              newData,
		Revision:          computeRevision(newData),
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
	var oldForges map[string]config.ForgeConfig
	var oldBackend config.SandboxBackendKind
	if oldCfg != nil {
		oldForges = oldCfg.Gateway.Forges
		oldBackend = oldCfg.Sandbox.Backend
	}
	// oldCfgSafe backs the generic restart-required-field loop below —
	// never nil, so restartFieldExtractors' funcs can dereference oldCfg's
	// sub-structs unconditionally. Mirrors the same "oldCfg nil only on the
	// defensive first-ever-call path" contract oldForges/oldBackend above
	// already follow.
	oldCfgSafe := oldCfg
	if oldCfgSafe == nil {
		oldCfgSafe = &config.Config{}
	}

	// sandbox.allowed_domains / notify.command / web.public_url used to be
	// hot-applied here (ReloadDynamic): sandbox.allowed_domains swapped
	// s.cfg.AllowedDomains, which dispatcher.Runner.AllowedDomains re-read
	// on every dispatch via a func() []string getter (BLOCKER 2, codex
	// review round 1); notify.command/web.public_url called
	// notify.Service.Update. PR #830 round-4 simplification (nose
	// directive) reclassified all three as ReloadRestartRequired — see
	// ReloadDynamic's own doc comment (internal/config/schema.go) for why:
	// that hot-reload machinery took 4 codex review rounds and introduced a
	// Server.Stop/dispatch deadlock (round 4 blocker 2, since
	// Runner.Dispatch called (*Server).AllowedDomains, which locked s.mu —
	// the same lock Server.Stop holds while waiting on in-flight dispatches
	// to finish). All three now fall through to the generic
	// ReloadRestartRequired loop below (restartFieldExtractors has entries
	// for all three) — this function no longer touches s.cfg or s.notifySvc
	// at all, and (*Server).AllowedDomains/s.notifySvc.Update's config-edit
	// call sites are gone entirely.

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
	//
	// gateway.forges.*.{host,forge,secret_key} and gateway.hosts are the two
	// documented exceptions (restartFieldExtractorExemptions below) — the
	// per-leaf changedForgeLeaves diff above already covers both with finer
	// granularity than this generic loop could.
	//
	// Coverage is verified exhaustively at daemon startup
	// (verifyRestartExtractorCoverage, called from wire.go's buildRuntime
	// before mountRoutes ever registers a route) — codex review round 3:
	// this loop used to panic on an uncovered leaf itself, but that panic
	// fired AFTER applyConfigYAMLLocked had already written the new
	// document to disk and swapped s.liveConfig, so an incomplete-coverage
	// bug would durably persist a config mutation and only then fail the
	// request that caused it. The branch below is therefore unreachable in
	// practice — coverage that is complete at startup stays complete
	// (config.Schema/restartFieldExtractors/restartFieldExtractorExemptions
	// are all static package vars, not mutated at runtime outside tests) —
	// and stays a warn-only defense-in-depth rather than a panic, so it
	// degrades to "no restart warning for this one leaf" instead of a
	// request-time 500 if it is ever somehow reached anyway.
	for _, spec := range config.Schema {
		if spec.Reload != config.ReloadRestartRequired {
			continue
		}
		extract, ok := restartFieldExtractors[spec.Path]
		if !ok {
			if _, exempt := restartFieldExtractorExemptions[spec.Path]; exempt {
				continue
			}
			slog.Warn("config: schema leaf is ReloadRestartRequired but registered in neither restartFieldExtractors nor restartFieldExtractorExemptions; startup coverage verification should have caught this — no restart-required warning will be produced for it",
				"path", spec.Path)
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
// gateway.forges.* and gateway.hosts are deliberately absent — see
// restartFieldExtractorExemptions below for why, and for the exhaustive-
// coverage contract that keeps this map (or that one) in sync with Schema.
//
// sandbox.allowed_domains/notify.command render their []string value joined
// on "\x00" (a byte that can never appear in a hostname or a shell argv
// element, so two distinct slices can never render to the same string) —
// the same "collapse to a comparable string" trick the scalar entries below
// use via strconv/.String(), just for a slice instead of a scalar. All
// three were ReloadDynamic before the PR #830 round-4 simplification (nose
// directive) — see ReloadDynamic's own doc comment (schema.go).
var restartFieldExtractors = map[string]func(*config.Config) string{
	"gc.enabled":                func(c *config.Config) string { return strconv.FormatBool(c.GC.Enabled) },
	"gc.interval":               func(c *config.Config) string { return c.GC.Interval.String() },
	"gc.older_than":             func(c *config.Config) string { return c.GC.OlderThan.String() },
	"web.http_addr":             func(c *config.Config) string { return c.Web.HTTPAddr },
	"web.public_url":            func(c *config.Config) string { return c.Web.PublicURL },
	"task_ask.disconnect_grace": func(c *config.Config) string { return c.TaskAsk.DisconnectGrace.String() },
	"notify.command":            func(c *config.Config) string { return strings.Join(c.Notify.Command, "\x00") },
	"sandbox.allowed_domains":   func(c *config.Config) string { return strings.Join(c.Sandbox.AllowedDomains, "\x00") },
}

// restartFieldExtractorExemptions documents every ReloadRestartRequired
// Schema leaf that deliberately has NO restartFieldExtractors entry, and
// why — the generic loop in applyDynamicConfigLocked panics on any
// ReloadRestartRequired leaf found in neither map (regression concern,
// codex review round 2), so a leaf's absence from restartFieldExtractors
// must always be a documented, intentional choice recorded here, never a
// silent gap. TestRestartFieldExtractors_ExhaustiveCoverage
// (config_edit_internal_test.go) pins that every current Schema leaf
// satisfies this at `go test` time, not just at runtime.
var restartFieldExtractorExemptions = map[string]string{
	// changedForgeLeaves's per-id, per-field diff (called separately, just
	// above the generic loop) already gives finer-grained warnings (MINOR 2,
	// codex review round 1) than comparing a single wildcard schema entry
	// ever could — it names which of host/forge/secret_key changed, not
	// just "something under gateway.forges.<id> changed".
	"gateway.forges.*.host":       "covered by changedForgeLeaves' per-id, per-field diff (finer-grained than a wildcard comparison)",
	"gateway.forges.*.forge":      "covered by changedForgeLeaves' per-id, per-field diff (finer-grained than a wildcard comparison)",
	"gateway.forges.*.secret_key": "covered by changedForgeLeaves' per-id, per-field diff (finer-grained than a wildcard comparison)",
	// Config.UnmarshalYAML always folds gateway.hosts into Gateway.Forges
	// before this function ever runs, so Config retains no Hosts field of
	// its own for an extractor to read — any of its changes already surface
	// through the Forges diff above instead.
	"gateway.hosts": "Config retains no Hosts field after UnmarshalYAML folds it into Forges; changes surface through the Forges diff instead",
}

// verifyRestartExtractorCoverage panics if any config.Schema leaf classified
// config.ReloadRestartRequired is registered in neither restartFieldExtractors
// nor restartFieldExtractorExemptions — the same exhaustiveness
// TestRestartFieldExtractors_ExhaustiveCoverage pins at `go test` time,
// re-verified here at daemon startup (BLOCKER, codex review round 3).
//
// Called once from wire.go's buildRuntime, before srv.liveConfig/
// srv.configPath are set and therefore before GET/POST /api/config
// (`boid config get/set/unset/apply/edit`) can accept a single request.
// This is what makes "a future schema addition forgets to register its
// restart-required leaf" fail the daemon at startup instead of durably
// persisting a config mutation and panicking mid-request — see
// applyDynamicConfigLocked's own doc comment for the pre-fix ordering this
// closes.
func verifyRestartExtractorCoverage() {
	for _, spec := range config.Schema {
		if spec.Reload != config.ReloadRestartRequired {
			continue
		}
		_, hasExtractor := restartFieldExtractors[spec.Path]
		_, hasExemption := restartFieldExtractorExemptions[spec.Path]
		if !hasExtractor && !hasExemption {
			panic(fmt.Sprintf("config: schema leaf %q is ReloadRestartRequired but registered in neither restartFieldExtractors nor restartFieldExtractorExemptions — add one or the other (internal/server/config_edit.go)", spec.Path))
		}
	}
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

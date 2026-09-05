package orchestrator

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// workspaceDBConsolidationVersion is the schema_migrations version key used
// by MigrateWorkspaceYAMLToDB's own staging/committed bookkeeping.
const workspaceDBConsolidationVersion = "workspace_db_consolidation"

// MigrateWorkspaceYAMLToDB performs the one-time cutover from yaml-file-
// authority workspaces (DefaultWorkspaceDir()/*.yaml + kit yaml under
// kitsDir) to DB-authority workspaces (the `workspaces` table). Call this
// once at daemon startup, after internal/db/migrate.Apply(conn) has run.
//
// Idempotent: once schema_migrations records this version as state=committed,
// every subsequent call returns nil immediately without touching the
// filesystem or the DB.
//
// Crash recovery: if a previous call was interrupted between recording
// state=staging and reaching state=committed, the next call recomputes the
// preflight input_hash and compares it against the recorded one — a match
// rolls forward, a mismatch aborts with an error requiring manual
// intervention rather than risk silently mixing old and new state.
//
// The old workspace yaml files and kitsDir are never modified or deleted by
// this function.
func MigrateWorkspaceYAMLToDB(conn *sql.DB, workspaceDir, kitsDir string, projectRepo *ProjectRepository) error {
	current, err := readMigrationState(conn, workspaceDBConsolidationVersion)
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: read schema_migrations state: %w", err)
	}
	if current != nil && current.state == "committed" {
		return nil
	}

	// Preflight performs no DB writes, so any failure here (corrupt yaml, a
	// kit host_command name collision, an unresolvable workspace slug) leaves
	// the database exactly as it was.
	pre, err := preflightWorkspaceMigration(workspaceDir, kitsDir, projectRepo)
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: preflight: %w", err)
	}

	if current != nil && current.state == "staging" {
		// A state=staging row may have been recorded by an older binary that
		// hashed the preflight inputs with an earlier shape (no
		// WorkspaceKitRefs, or WorkspaceKitRefs but still with
		// AdditionalBindings). Comparing only against the current shape would
		// make such a row a guaranteed mismatch even when nothing on disk
		// actually changed, turning a routine binary upgrade into a mandatory
		// manual-intervention abort. pre.legacyInputHashPR6 and
		// pre.legacyInputHashPR7WithBindings recompute the same inputs under
		// those older shapes (see computeWorkspaceMigrationInputHashPR6Shape /
		// ...PR7WithBindingsShape) so an upgrade-in-place with unchanged
		// inputs still rolls forward; an actual on-disk change still aborts.
		if current.inputHash != pre.inputHash &&
			current.inputHash != pre.legacyInputHashPR6 &&
			current.inputHash != pre.legacyInputHashPR7WithBindings {
			return fmt.Errorf(
				"workspace_db_consolidation: found state=staging (input_hash=%q) from an interrupted prior attempt, but the current workspace/kit inputs hash to %q — refusing to roll forward automatically since the on-disk inputs changed since the interruption; restore the prior workspace yaml/kit state (or manually resolve the schema_migrations row) and restart (docs/plans/workspace-db-consolidation.md crash recovery)",
				current.inputHash, pre.inputHash,
			)
		}
		// Recorded input_hash matches what we'd compute right now (the
		// current PR4 shape, the legacy PR6 shape, or the legacy
		// PR7-with-bindings shape from an in-progress binary upgrade): safe
		// to roll forward by re-running the (idempotent) write phase below.
	}

	// Phase 1: record the staging attempt in its own committed transaction so
	// a crash during phase 2 leaves durable evidence for the crash-recovery
	// check above — folding this into phase 2's transaction would roll the
	// staging marker back along with an interrupted phase 2.
	tx1, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: begin staging tx: %w", err)
	}
	if err := upsertMigrationRow(tx1, workspaceDBConsolidationVersion, "staging", pre.inputHash); err != nil {
		_ = tx1.Rollback()
		return fmt.Errorf("workspace_db_consolidation: record staging: %w", err)
	}
	if err := tx1.Commit(); err != nil {
		return fmt.Errorf("workspace_db_consolidation: commit staging: %w", err)
	}

	// Phase 2: the actual cutover writes, all inside one transaction,
	// committed together with the final state=committed update. A
	// single-process daemon with a single pooled connection
	// (internal/db.Open sets MaxOpenConns(1)) makes a plain Begin() behave
	// like BEGIN IMMEDIATE here, so no driver-specific BEGIN IMMEDIATE is
	// needed.
	tx2, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: begin tx: %w", err)
	}
	defer func() { _ = tx2.Rollback() }() // no-op once committed

	for _, slug := range pre.sortedSlugs {
		if err := saveWorkspaceRow(tx2, slug, pre.workspaces[slug]); err != nil {
			return fmt.Errorf("workspace_db_consolidation: save workspace %q: %w", slug, err)
		}
	}
	if err := ensureDefaultWorkspaceRow(tx2); err != nil {
		return fmt.Errorf("workspace_db_consolidation: ensure default workspace: %w", err)
	}
	if err := verifyProjectWorkspaceRefsResolvable(tx2); err != nil {
		return fmt.Errorf("workspace_db_consolidation: %w", err)
	}

	hostCommandsPath, err := DefaultHostCommandsPath()
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: resolve host_commands.yaml path: %w", err)
	}
	// Only write the aggregated host_commands.yaml when no file exists there
	// yet — an existing (generated or hand-edited) config must not be
	// silently replaced by this migration's own freshly-aggregated spec.
	if _, err := writeHostCommandsConfigIfMissing(hostCommandsPath, pre.hostCommands); err != nil {
		return fmt.Errorf("workspace_db_consolidation: write host_commands.yaml: %w", err)
	}

	if err := upsertMigrationRow(tx2, workspaceDBConsolidationVersion, "committed", pre.inputHash); err != nil {
		return fmt.Errorf("workspace_db_consolidation: record committed: %w", err)
	}
	if err := tx2.Commit(); err != nil {
		return fmt.Errorf("workspace_db_consolidation: commit: %w", err)
	}
	return nil
}

// workspaceMigrationPreflight holds everything preflightWorkspaceMigration
// computed: the DB-bound workspace metas (HostCommands already unioned in
// from each workspace's Kits), the aggregated kit host_commands config, and
// the deterministic hash of every input consulted.
type workspaceMigrationPreflight struct {
	workspaces   map[string]*WorkspaceMeta
	sortedSlugs  []string
	hostCommands map[string]HostCommandSpec
	inputHash    string
	// legacyInputHashPR6 is the same preflight inputs hashed with an earlier
	// (pre-WorkspaceKitRefs) shape — see
	// computeWorkspaceMigrationInputHashPR6Shape's doc comment.
	legacyInputHashPR6 string
	// legacyInputHashPR7WithBindings is the same preflight inputs hashed with
	// an intermediate shape (WorkspaceKitRefs present, AdditionalBindings
	// still present) — see
	// computeWorkspaceMigrationInputHashPR7WithBindingsShape's doc comment.
	legacyInputHashPR7WithBindings string
}

// preflightWorkspaceMigration parses every workspace yaml under
// workspaceDir, aggregates every kit's host_commands under kitsDir, checks
// that every project's linked workspace resolves (to a parsed workspace or
// to DefaultWorkspaceSlug, which is always ensured to exist), and computes a
// deterministic hash over all of it. No side effects: this function performs
// no writes to conn or to disk.
func preflightWorkspaceMigration(workspaceDir, kitsDir string, projectRepo *ProjectRepository) (*workspaceMigrationPreflight, error) {
	// NewWorkspaceStore with no repository wired reads plain yaml — exactly
	// the pre-cutover behavior we want to reuse here as the source of truth.
	yamlStore := NewWorkspaceStore(workspaceDir)
	slugs, err := yamlStore.List()
	if err != nil {
		return nil, fmt.Errorf("list workspace yaml: %w", err)
	}

	// readWorkspaceYAMLSnapshot (below) needs the resolved directory, not the
	// possibly-empty parameter: NewWorkspaceStore("") resolves internally to
	// DefaultWorkspaceDir() on its own private field, which this function
	// has no access to — resolving it again here the same way keeps
	// readWorkspaceYAMLSnapshot reading the same files yamlStore.List() just
	// enumerated, instead of a bogus path relative to the daemon's cwd.
	resolvedWorkspaceDir := workspaceDir
	if resolvedWorkspaceDir == "" {
		if d, dirErr := DefaultWorkspaceDir(); dirErr == nil {
			resolvedWorkspaceDir = d
		}
	}

	rawWorkspaces := make(map[string]*WorkspaceMeta, len(slugs))
	rawKitRefs := make(map[string][]string, len(slugs))
	// rawAdditionalBindings is each workspace's raw additional_bindings list
	// (WorkspaceMeta itself no longer carries this field) — kept only so
	// computeWorkspaceMigrationInputHashPR6Shape can replay an older binary's
	// hash. Never fed into materializeKitRuntimeIntoWorkspace or any DB-bound
	// WorkspaceMeta below.
	rawAdditionalBindings := make(map[string][]BindMount, len(slugs))
	for _, slug := range slugs {
		// readWorkspaceYAMLSnapshot reads slug.yaml exactly once and derives
		// meta, kitRefs, and additionalBindings from that single byte
		// snapshot, avoiding a TOCTOU where an atomic rename between two
		// independent reads could hand this migration a hybrid that never
		// existed on disk at any single instant.
		meta, kitRefs, additionalBindings, err := readWorkspaceYAMLSnapshot(resolvedWorkspaceDir, slug)
		if err != nil {
			return nil, fmt.Errorf("read workspace yaml %q: %w", slug, err)
		}
		rawWorkspaces[slug] = meta
		rawKitRefs[slug] = kitRefs
		rawAdditionalBindings[slug] = additionalBindings
	}

	// Read every installed kit's kit.yaml exactly once into an immutable
	// snapshot, then derive both the aggregated host_commands config and
	// every workspace's kit-materialized runtime from that single snapshot —
	// avoiding a kit.yaml edit racing between two independent re-reads (this
	// preflight runs at most once successfully, so any such inconsistency
	// would be permanent).
	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot kit yaml: %w", err)
	}
	hostCommands, err := aggregateHostCommandsFromSnapshot(snap)
	if err != nil {
		return nil, fmt.Errorf("aggregate kit host_commands: %w", err)
	}

	// Project -> workspace reference check: every referenced workspace_id
	// must either resolve to a parsed workspace yaml or be
	// DefaultWorkspaceSlug (which the write phase always ensures exists).
	referenced, err := projectRepo.ListProjectWorkspaceReferences()
	if err != nil {
		return nil, fmt.Errorf("list project workspace references: %w", err)
	}
	for _, ws := range referenced {
		if ws.ID == DefaultWorkspaceSlug {
			continue
		}
		if _, ok := rawWorkspaces[ws.ID]; !ok {
			return nil, fmt.Errorf(
				"%d project(s) reference workspace %q, which has no corresponding workspace yaml under %s",
				ws.ProjectCount, ws.ID, workspaceDir,
			)
		}
	}

	inputHash, err := computeWorkspaceMigrationInputHash(rawWorkspaces, hostCommands, referenced, snap.byKit, rawKitRefs)
	if err != nil {
		return nil, fmt.Errorf("compute input hash: %w", err)
	}

	// Also hash the same raw inputs under two earlier shapes, purely so
	// MigrateWorkspaceYAMLToDB's crash-recovery check can roll forward a
	// state=staging row recorded by an older binary whose on-disk inputs
	// have not actually changed since — see computeWorkspaceMigrationInputHashPR6Shape's
	// and ...PR7WithBindingsShape's doc comments. rawKitRefs/
	// rawAdditionalBindings are passed through because current WorkspaceMeta
	// no longer carries those fields for rawWorkspaces to supply them from.
	legacyInputHashPR6, err := computeWorkspaceMigrationInputHashPR6Shape(rawWorkspaces, hostCommands, referenced, snap.byKit, rawKitRefs, rawAdditionalBindings)
	if err != nil {
		return nil, fmt.Errorf("compute legacy (pre-PR7) input hash: %w", err)
	}

	legacyInputHashPR7WithBindings, err := computeWorkspaceMigrationInputHashPR7WithBindingsShape(rawWorkspaces, hostCommands, referenced, snap.byKit, rawKitRefs, rawAdditionalBindings)
	if err != nil {
		return nil, fmt.Errorf("compute legacy (PR7-with-bindings) input hash: %w", err)
	}

	// Build the DB-bound workspace metas: same data as rawWorkspaces, but
	// with HostCommands/Env filled in from each workspace's legacy kit refs.
	// Cloned rather than mutated in place so the hash computed above
	// reflects only the raw, unexpanded yaml/kit inputs.
	dbWorkspaces := make(map[string]*WorkspaceMeta, len(rawWorkspaces))
	for slug, raw := range rawWorkspaces {
		meta := cloneWorkspaceMetaForMigration(raw)
		if err := materializeKitRuntimeIntoWorkspace(snap, rawKitRefs[slug], meta); err != nil {
			return nil, fmt.Errorf("workspace %q: materialize kit runtime: %w", slug, err)
		}
		dbWorkspaces[slug] = meta
	}

	sortedSlugs := make([]string, 0, len(dbWorkspaces))
	for slug := range dbWorkspaces {
		sortedSlugs = append(sortedSlugs, slug)
	}
	sort.Strings(sortedSlugs)

	return &workspaceMigrationPreflight{
		workspaces:                     dbWorkspaces,
		sortedSlugs:                    sortedSlugs,
		hostCommands:                   hostCommands,
		inputHash:                      inputHash,
		legacyInputHashPR6:             legacyInputHashPR6,
		legacyInputHashPR7WithBindings: legacyInputHashPR7WithBindings,
	}, nil
}

// cloneWorkspaceMetaForMigration returns a shallow copy of meta. Nothing
// this migration does mutates a slice/map field in place (every merge
// helper — unionStringsSorted / mergeStringMaps / unionBindMountSlices —
// returns a brand-new slice/map rather than mutating its input), so a
// shallow copy — which leaves every field's slice/map initially sharing the
// original's backing array — is safe: nothing is ever mutated in place
// through the clone.
func cloneWorkspaceMetaForMigration(meta *WorkspaceMeta) *WorkspaceMeta {
	clone := *meta
	return &clone
}

// kitRuntimeRaw holds the raw (unexpanded) host_commands / env /
// additional_bindings sections read directly from a single kit.yaml file.
// Used only by snapshotAllKitYAMLs.
type kitRuntimeRaw struct {
	HostCommands       HostCommands      `yaml:"host_commands"`
	Env                map[string]string `yaml:"env"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings"`
}

// readKitRuntimeRaw reads kitDir's kit.yaml and returns its
// host_commands/env/additional_bindings sections unexpanded. Values are
// deliberately left raw for the same reason readKitHostCommandsRaw does:
// expanding here would (a) bake resolved host-env values (potentially
// secret-shaped) into the workspaces table and (b) let two kits using
// differently-named placeholders that happen to resolve to the same value
// silently evade the host_commands collision check below.
func readKitRuntimeRaw(kitDir string) (kitRuntimeRaw, error) {
	yamlPath := filepath.Join(kitDir, "kit.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return kitRuntimeRaw{}, fmt.Errorf("read kit.yaml: %w", err)
	}
	var raw kitRuntimeRaw
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return kitRuntimeRaw{}, fmt.Errorf("parse kit.yaml: %w", err)
	}
	return raw, nil
}

// workspaceYAMLReadFile reads a workspace yaml file's raw bytes. Indirected
// through a package-level variable rather than calling os.ReadFile directly
// solely so tests can pin readWorkspaceYAMLSnapshot's core TOCTOU-avoidance
// invariant below — that it reads the file exactly once — by counting calls
// and/or swapping the underlying file's content out from under a would-be
// second read.
var workspaceYAMLReadFile = os.ReadFile

// readWorkspaceYAMLSnapshot reads workspaceDir/slug.yaml's raw bytes exactly
// once and decodes its WorkspaceMeta fields, its legacy top-level `kits:`
// reference list, and its (also retired) top-level `additional_bindings:`
// list from that single byte snapshot — reading once and decoding all three
// shapes from it avoids a TOCTOU where an atomic rename between independent
// reads of the same file could hand this one-time migration a hybrid that
// never existed on disk at any single instant.
//
// additionalBindings has exactly one consumer: preflightWorkspaceMigration
// passes it to computeWorkspaceMigrationInputHashPR6Shape so an older
// binary's crash-recovery hash can still be replayed byte-for-byte; it is
// never materialized into a DB-bound WorkspaceMeta.
//
// An absent `kits:` or `additional_bindings:` key decodes to a nil slice —
// the fast path, and the common case for anything authored post-cutover.
func readWorkspaceYAMLSnapshot(workspaceDir, slug string) (meta *WorkspaceMeta, kitRefs []string, additionalBindings []BindMount, err error) {
	path := filepath.Join(workspaceDir, slug+".yaml")
	raw, err := workspaceYAMLReadFile(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	meta = &WorkspaceMeta{}
	if err := yaml.Unmarshal(raw, meta); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var kitsDoc struct {
		Kits []string `yaml:"kits"`
	}
	if err := yaml.Unmarshal(raw, &kitsDoc); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var bindingsDoc struct {
		AdditionalBindings []BindMount `yaml:"additional_bindings"`
	}
	if err := yaml.Unmarshal(raw, &bindingsDoc); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// additionalBindings is never materialized onto a DB-bound WorkspaceMeta,
	// so warn when a workspace yaml has a non-empty additional_bindings: key
	// — otherwise it is silently discarded from the operator's perspective.
	if present, presentErr := additionalBindingsKeyPresent(raw); presentErr == nil && present {
		slog.Warn("workspace: additional_bindings is no longer supported (retired in docs/plans/home-workspace-volume.md Phase 4 PR4); ignoring",
			"workspace", slug, "path", path)
	}
	return meta, kitsDoc.Kits, bindingsDoc.AdditionalBindings, nil
}

// kitYAMLSnapshot is an immutable, once-only read of every installed kit's
// kit.yaml host_commands/env/additional_bindings sections, keyed by kit
// directory name. preflightWorkspaceMigration builds this exactly once per
// call and derives both the aggregated host_commands config
// (aggregateHostCommandsFromSnapshot) and every workspace's kit-materialized
// runtime (materializeKitRuntimeIntoWorkspace) from it, avoiding a kit.yaml
// edit racing between two independent re-reads.
type kitYAMLSnapshot struct {
	kitsDir     string
	byKit       map[string]kitRuntimeRaw
	sortedNames []string // kit dir names with a kit.yaml, sorted — deterministic conflict error messages
}

// snapshotAllKitYAMLs scans kitsDir for installed kits (subdirectories
// containing a kit.yaml) and reads each one's host_commands/env/
// additional_bindings exactly once. A missing kitsDir is not an error — it
// returns an empty snapshot, matching LoadHostCommandsFromKits' "空扱い"
// contract.
func snapshotAllKitYAMLs(kitsDir string) (*kitYAMLSnapshot, error) {
	entries, err := os.ReadDir(kitsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &kitYAMLSnapshot{kitsDir: kitsDir, byKit: map[string]kitRuntimeRaw{}}, nil
		}
		return nil, fmt.Errorf("list kits dir %q: %w", kitsDir, err)
	}

	// Sort subdirectory names up front so both derived views (aggregate and
	// per-workspace union) — and any error messages they produce — are
	// deterministic regardless of os.ReadDir's or the filesystem's
	// iteration order, mirroring LoadHostCommandsFromKits.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	byKit := make(map[string]kitRuntimeRaw, len(names))
	var sortedNames []string
	for _, name := range names {
		kitDir := filepath.Join(kitsDir, name)
		if _, err := os.Stat(filepath.Join(kitDir, "kit.yaml")); err != nil {
			if os.IsNotExist(err) {
				continue // not a kit directory (no kit.yaml)
			}
			return nil, fmt.Errorf("stat %q: %w", filepath.Join(kitDir, "kit.yaml"), err)
		}
		raw, err := readKitRuntimeRaw(kitDir)
		if err != nil {
			return nil, fmt.Errorf("read kit %q: %w", kitDir, err)
		}
		byKit[name] = raw
		sortedNames = append(sortedNames, name)
	}
	return &kitYAMLSnapshot{kitsDir: kitsDir, byKit: byKit, sortedNames: sortedNames}, nil
}

// aggregateHostCommandsFromSnapshot replicates LoadHostCommandsFromKits'
// aggregation logic (dedupe identical definitions across kits, error on
// differing definitions for the same name) but reads from an already-taken
// kitYAMLSnapshot instead of re-scanning kitsDir. Kept as a private,
// migration-only function rather than changing LoadHostCommandsFromKits'
// public signature, since that function is also called independently by
// internal/server/wire.go's own preflight.
func aggregateHostCommandsFromSnapshot(snap *kitYAMLSnapshot) (map[string]HostCommandSpec, error) {
	aggregated := make(map[string]HostCommandSpec)
	definedBy := make(map[string]string) // command name -> kit dir that first defined it

	for _, name := range snap.sortedNames {
		hostCommands := snap.byKit[name].HostCommands

		// Iterate command names in sorted order for the same determinism
		// reason as LoadHostCommandsFromKits.
		cmdNames := make([]string, 0, len(hostCommands))
		for cmdName := range hostCommands {
			cmdNames = append(cmdNames, cmdName)
		}
		sort.Strings(cmdNames)

		for _, cmdName := range cmdNames {
			spec := normalizeHostCommandSpec(hostCommands[cmdName])
			existing, ok := aggregated[cmdName]
			if !ok {
				aggregated[cmdName] = spec
				definedBy[cmdName] = name
				continue
			}
			if reflect.DeepEqual(existing, spec) {
				continue // dedupe: identical definition across kits, ok
			}
			return nil, fmt.Errorf(
				"host_commands: command %q is defined differently by kit %q and kit %q; align the definitions or rename one",
				cmdName, filepath.Join(snap.kitsDir, definedBy[cmdName]), filepath.Join(snap.kitsDir, name),
			)
		}
	}
	return aggregated, nil
}

// materializeKitRuntimeIntoWorkspace unions the named kits' raw
// host_commands names and Env into meta (mutated in place), reading every
// kit's data from the already-taken snap rather than the filesystem.
//
// A kit's AdditionalBindings is no longer unioned in: the workspace-scoped
// AdditionalBindings mechanism it used to feed is retired (WorkspaceMeta has
// no field for it any more). kitRuntimeRaw still carries that field purely
// so the input-hash functions below can detect a kit.yaml
// additional_bindings-only edit as an input change.
//
// Precedence: kit-provided Env is a default, meta's own (workspace-authored)
// Env wins on conflict.
//
// A kit name with no corresponding entry in snap aborts the migration
// (rather than warn-and-skip) — see the error return below for why.
//
// Values are taken raw/unexpanded (a kit env value containing a literal
// ${VAR} placeholder is stored as-is); dispatch-time hydration
// (ProjectStore.GetWithWorkspace) expands such placeholders later via
// expandWorkspaceRuntimeForDispatch (workspace_meta.go).
func materializeKitRuntimeIntoWorkspace(snap *kitYAMLSnapshot, kits []string, meta *WorkspaceMeta) error {
	if len(kits) == 0 {
		return nil
	}

	seenHostCommandNames := make(map[string]struct{})
	var kitHostCommandNames []string
	kitEnv := make(map[string]string)

	for _, kitName := range kits {
		raw, ok := snap.byKit[kitName]
		if !ok {
			// Abort instead of warn-and-skip: this migration commits at most
			// once, so a kit directory merely temporarily missing at cutover
			// time would otherwise permanently strand this workspace's
			// kit-supplied env/host_commands with no way to recover except
			// hand-editing the workspaces table. Failing preflight instead
			// leaves zero DB changes, so the operator can fix the kit
			// directory (or its reference) and restart.
			kitDir := filepath.Join(snap.kitsDir, kitName)
			return fmt.Errorf(
				"kit %q has no kit.yaml at %s; restore the kit directory (or remove %q from this workspace's kits list) and restart the daemon",
				kitName, kitDir, kitName,
			)
		}
		for name := range raw.HostCommands {
			if _, seen := seenHostCommandNames[name]; seen {
				continue
			}
			seenHostCommandNames[name] = struct{}{}
			kitHostCommandNames = append(kitHostCommandNames, name)
		}
		// Env: later kit overrides earlier kit — same order MergeKitRuntime
		// applies for the multi-kit case at dispatch time.
		for k, v := range raw.Env {
			kitEnv[k] = v
		}
	}

	sort.Strings(kitHostCommandNames)
	meta.HostCommands = unionStringsSorted(meta.HostCommands, kitHostCommandNames)

	if len(kitEnv) > 0 {
		// meta.Env (workspace-authored) wins over kit-supplied defaults.
		meta.Env = mergeStringMaps(kitEnv, meta.Env)
	}

	return nil
}

// MaterializeWorkspaceKitsForPersist resolves kitRefs (a legacy `kits:`
// reference list, sourced by the caller) against the kits installed under
// kitsDir, merging their host_commands and env into meta in place — the
// same expansion MigrateWorkspaceYAMLToDB performs once at cutover.
// WorkspaceMeta has no Kits field of its own, so callers (e.g.
// cmd/workspace.go's ensureWorkspaceExistsForAssign) must source kitRefs
// themselves, typically from a raw on-disk legacy yaml. Without this
// expansion step, a workspace referencing a kit would silently lose that
// kit's host_commands/env on save, since the workspaces table has no column
// to carry an unresolved kit reference.
//
// len(kitRefs) == 0 is a fast path: the overwhelming majority of calls (any
// workspace that never referenced a kit) never touch the filesystem at all.
func MaterializeWorkspaceKitsForPersist(kitsDir string, kitRefs []string, meta *WorkspaceMeta) error {
	if meta == nil || len(kitRefs) == 0 {
		return nil
	}
	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		return fmt.Errorf("snapshot kit yaml: %w", err)
	}
	return materializeKitRuntimeIntoWorkspace(snap, kitRefs, meta)
}

// unionStringsSorted returns the sorted, deduplicated union of a and b.
func unionStringsSorted(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// workspaceMigrationHashInput is the canonical shape hashed by
// computeWorkspaceMigrationInputHash. All four fields are maps, and
// encoding/json has sorted map keys when marshaling since Go 1.12, so
// json.Marshal of this struct is deterministic across runs regardless of Go
// map iteration order — no manual key-sorting is needed beyond that.
type workspaceMigrationHashInput struct {
	Workspaces           map[string]*WorkspaceMeta  `json:"workspaces"`
	HostCommands         map[string]HostCommandSpec `json:"host_commands"`
	ProjectWorkspaceRefs []*WorkspaceSummary        `json:"project_workspace_refs"`
	// KitRuntime is every installed kit's raw host_commands/env/
	// additional_bindings snapshot (kitYAMLSnapshot.byKit), keyed by kit dir
	// name — included so a kit.yaml edit that changes only its env/
	// additional_bindings section (not reflected anywhere else in this
	// struct) is not silently missed by the crash-recovery hash comparison.
	KitRuntime map[string]kitRuntimeRaw `json:"kit_runtime"`
	// WorkspaceKitRefs is each workspace's legacy `kits:` reference list,
	// read directly off the raw yaml file since WorkspaceMeta itself no
	// longer has a Kits field to carry it — included for the same
	// hash-completeness reason as KitRuntime above.
	WorkspaceKitRefs map[string][]string `json:"workspace_kit_refs"`
}

// computeWorkspaceMigrationInputHash hashes every input
// preflightWorkspaceMigration consulted (raw workspace metas, aggregated kit
// host_commands, project->workspace reference list, each kit's raw runtime
// snapshot, each workspace's legacy kit ref list) into a single sha256 hex
// digest, used by MigrateWorkspaceYAMLToDB's crash recovery to detect
// whether the on-disk/DB inputs changed since an interrupted attempt.
func computeWorkspaceMigrationInputHash(
	workspaces map[string]*WorkspaceMeta,
	hostCommands map[string]HostCommandSpec,
	projectRefs []*WorkspaceSummary,
	kitRuntime map[string]kitRuntimeRaw,
	workspaceKitRefs map[string][]string,
) (string, error) {
	b, err := json.Marshal(workspaceMigrationHashInput{
		Workspaces:           workspaces,
		HostCommands:         hostCommands,
		ProjectWorkspaceRefs: sortedWorkspaceRefsForHash(projectRefs),
		KitRuntime:           kitRuntime,
		WorkspaceKitRefs:     workspaceKitRefs,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// sortedWorkspaceRefsForHash returns a copy of projectRefs sorted by ID, so
// the hash computed from it is deterministic regardless of the caller's
// (DB query) iteration order. Shared by
// computeWorkspaceMigrationInputHash and its PR6-shape counterpart below.
func sortedWorkspaceRefsForHash(projectRefs []*WorkspaceSummary) []*WorkspaceSummary {
	sorted := append([]*WorkspaceSummary(nil), projectRefs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted
}

// pr6WorkspaceMeta mirrors WorkspaceMeta (workspace_meta.go) field-for-field,
// tag-for-tag, order-for-order, exactly as it existed before the Kits field
// was removed from WorkspaceMeta outright. Used ONLY by
// computeWorkspaceMigrationInputHashPR6Shape for legacy hash reconstruction.
// Go's encoding/json marshals struct fields in declaration order (unlike map
// keys, which it sorts), so this field order is not cosmetic.
//
// IMPORTANT: do NOT modify this struct — its byte shape must stay stable
// forever to keep the crash-recovery upgrade path deterministic for any
// state=staging row an older binary may have left on disk.
type pr6WorkspaceMeta struct {
	Kits               []string          `yaml:"kits,omitempty" json:"kits,omitempty"`
	Env                map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Capabilities       Capabilities      `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	AllowedDomains     []string          `yaml:"allowed_domains,omitempty" json:"allowed_domains,omitempty"`
	ExtraRepos         []string          `yaml:"extra_repos,omitempty" json:"extra_repos,omitempty"`
	HostCommands       []string          `yaml:"host_commands,omitempty" json:"host_commands,omitempty"`
	ContainerImage     string            `yaml:"container_image,omitempty" json:"container_image,omitempty"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings,omitempty" json:"additional_bindings,omitempty"`
}

// workspaceMigrationHashInputPR6 mirrors workspaceMigrationHashInput exactly
// as it existed before the WorkspaceKitRefs field was added — used only by
// computeWorkspaceMigrationInputHashPR6Shape for the crash-recovery upgrade
// check. IMPORTANT: do NOT add WorkspaceKitRefs (or any future field
// workspaceMigrationHashInput gains) here — this type must keep reproducing
// the exact byte shape an older binary would have hashed, forever.
//
// Workspaces is keyed to pr6WorkspaceMeta (which still carries Kits
// directly) rather than the current, Kits-less WorkspaceMeta — otherwise a
// workspace that referenced a kit would hash differently from what an
// older binary computed for the identical on-disk inputs.
type workspaceMigrationHashInputPR6 struct {
	Workspaces           map[string]*pr6WorkspaceMeta `json:"workspaces"`
	HostCommands         map[string]HostCommandSpec   `json:"host_commands"`
	ProjectWorkspaceRefs []*WorkspaceSummary          `json:"project_workspace_refs"`
	KitRuntime           map[string]kitRuntimeRaw     `json:"kit_runtime"`
}

// computeWorkspaceMigrationInputHashPR6Shape recomputes
// preflightWorkspaceMigration's input hash using an earlier shape (no
// WorkspaceKitRefs field, and each workspace rehydrated with its Kits field
// restored from workspaceKitRefs) from the same raw inputs
// computeWorkspaceMigrationInputHash was given.
//
// Why this exists: MigrateWorkspaceYAMLToDB's crash recovery persists
// input_hash across a daemon binary upgrade. An older binary that recorded
// state=staging (interrupted mid-migration) computed its hash under an
// earlier shape; restarting on a newer binary would otherwise always
// compute a different hash for identical on-disk inputs, turning every such
// upgrade into a spurious "inputs changed, refusing to roll forward" abort.
// Comparing the recorded hash against both shapes lets a genuine
// upgrade-in-place roll forward while still catching an actual on-disk
// change. workspaceKitRefs/workspaceAdditionalBindings restore the fields
// current WorkspaceMeta no longer carries, since neither
// pr6WorkspaceMeta.Kits nor .AdditionalBindings can be populated from
// rawWorkspaces any more.
func computeWorkspaceMigrationInputHashPR6Shape(
	workspaces map[string]*WorkspaceMeta,
	hostCommands map[string]HostCommandSpec,
	projectRefs []*WorkspaceSummary,
	kitRuntime map[string]kitRuntimeRaw,
	workspaceKitRefs map[string][]string,
	workspaceAdditionalBindings map[string][]BindMount,
) (string, error) {
	pr6Workspaces := make(map[string]*pr6WorkspaceMeta, len(workspaces))
	for slug, meta := range workspaces {
		pr6Workspaces[slug] = &pr6WorkspaceMeta{
			Kits:               workspaceKitRefs[slug],
			Env:                meta.Env,
			Capabilities:       meta.Capabilities,
			AllowedDomains:     meta.AllowedDomains,
			ExtraRepos:         meta.ExtraRepos,
			HostCommands:       meta.HostCommands,
			ContainerImage:     meta.ContainerImage,
			AdditionalBindings: workspaceAdditionalBindings[slug],
		}
	}
	b, err := json.Marshal(workspaceMigrationHashInputPR6{
		Workspaces:           pr6Workspaces,
		HostCommands:         hostCommands,
		ProjectWorkspaceRefs: sortedWorkspaceRefsForHash(projectRefs),
		KitRuntime:           kitRuntime,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// pr7WorkspaceMetaWithBindings mirrors WorkspaceMeta (workspace_meta.go)
// field-for-field, tag-for-tag, order-for-order, exactly as it existed after
// the Kits field was removed (pr6WorkspaceMeta above still has it) but
// before AdditionalBindings was removed outright. Used ONLY by
// computeWorkspaceMigrationInputHashPR7WithBindingsShape for legacy hash
// reconstruction. Go's encoding/json marshals struct fields in declaration
// order (unlike map keys, which it sorts), so this field order is not
// cosmetic.
//
// IMPORTANT: do NOT modify this struct — same discipline as pr6WorkspaceMeta
// above: its byte shape must stay stable forever to keep the crash-recovery
// upgrade path deterministic for any state=staging row an older binary may
// have left on disk.
type pr7WorkspaceMetaWithBindings struct {
	Env                map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Capabilities       Capabilities      `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	AllowedDomains     []string          `yaml:"allowed_domains,omitempty" json:"allowed_domains,omitempty"`
	ExtraRepos         []string          `yaml:"extra_repos,omitempty" json:"extra_repos,omitempty"`
	HostCommands       []string          `yaml:"host_commands,omitempty" json:"host_commands,omitempty"`
	ContainerImage     string            `yaml:"container_image,omitempty" json:"container_image,omitempty"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings,omitempty" json:"additional_bindings,omitempty"`
}

// workspaceMigrationHashInputPR7WithBindings mirrors workspaceMigrationHashInput
// exactly as it existed at an intermediate point: the WorkspaceKitRefs field
// is present (unlike workspaceMigrationHashInputPR6 above), but Workspaces is
// keyed to pr7WorkspaceMetaWithBindings since WorkspaceMeta still carried
// AdditionalBindings at that point. Used only by
// computeWorkspaceMigrationInputHashPR7WithBindingsShape for the
// crash-recovery upgrade check. IMPORTANT: do NOT add any field
// workspaceMigrationHashInput gains later here — this type must keep
// reproducing the exact byte shape that older binary would have hashed,
// forever.
type workspaceMigrationHashInputPR7WithBindings struct {
	Workspaces           map[string]*pr7WorkspaceMetaWithBindings `json:"workspaces"`
	HostCommands         map[string]HostCommandSpec               `json:"host_commands"`
	ProjectWorkspaceRefs []*WorkspaceSummary                      `json:"project_workspace_refs"`
	KitRuntime           map[string]kitRuntimeRaw                 `json:"kit_runtime"`
	WorkspaceKitRefs     map[string][]string                      `json:"workspace_kit_refs"`
}

// computeWorkspaceMigrationInputHashPR7WithBindingsShape recomputes
// preflightWorkspaceMigration's input hash using an intermediate shape
// (WorkspaceKitRefs present, and each workspace rehydrated with its
// AdditionalBindings field restored from workspaceAdditionalBindings) from
// the same raw inputs computeWorkspaceMigrationInputHash was given.
//
// Why this exists: current WorkspaceMeta has no AdditionalBindings field any
// more, but a binary built between the WorkspaceKitRefs addition and its
// removal would have recorded a state=staging hash under a shape that still
// had it. computeWorkspaceMigrationInputHashPR6Shape's fallback alone
// doesn't cover that binary (it predates WorkspaceKitRefs entirely), so this
// third shape closes the remaining gap: comparing the recorded hash against
// it too lets a genuine upgrade-in-place roll forward while still catching
// an actual on-disk change.
func computeWorkspaceMigrationInputHashPR7WithBindingsShape(
	workspaces map[string]*WorkspaceMeta,
	hostCommands map[string]HostCommandSpec,
	projectRefs []*WorkspaceSummary,
	kitRuntime map[string]kitRuntimeRaw,
	workspaceKitRefs map[string][]string,
	workspaceAdditionalBindings map[string][]BindMount,
) (string, error) {
	pr7Workspaces := make(map[string]*pr7WorkspaceMetaWithBindings, len(workspaces))
	for slug, meta := range workspaces {
		pr7Workspaces[slug] = &pr7WorkspaceMetaWithBindings{
			Env:                meta.Env,
			Capabilities:       meta.Capabilities,
			AllowedDomains:     meta.AllowedDomains,
			ExtraRepos:         meta.ExtraRepos,
			HostCommands:       meta.HostCommands,
			ContainerImage:     meta.ContainerImage,
			AdditionalBindings: workspaceAdditionalBindings[slug],
		}
	}
	b, err := json.Marshal(workspaceMigrationHashInputPR7WithBindings{
		Workspaces:           pr7Workspaces,
		HostCommands:         hostCommands,
		ProjectWorkspaceRefs: sortedWorkspaceRefsForHash(projectRefs),
		KitRuntime:           kitRuntime,
		WorkspaceKitRefs:     workspaceKitRefs,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// verifyProjectWorkspaceRefsResolvable re-checks, from inside the write
// transaction, that every project_workspaces.workspace_id resolves to a row
// now present in workspaces. This deliberately duplicates
// preflightWorkspaceMigration's earlier check, which ran before any
// workspace row existed in the DB — this later check runs after every
// workspace has been written, inside the same transaction, so it is immune
// to a stale read from outside the transaction.
func verifyProjectWorkspaceRefsResolvable(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT pw.project_id, pw.workspace_id
		FROM project_workspaces pw
		LEFT JOIN workspaces w ON w.slug = pw.workspace_id
		WHERE w.slug IS NULL
		ORDER BY pw.project_id`)
	if err != nil {
		return fmt.Errorf("verify project workspace references: %w", err)
	}
	defer rows.Close()

	var broken []string
	for rows.Next() {
		var projectID, workspaceID string
		if err := rows.Scan(&projectID, &workspaceID); err != nil {
			return fmt.Errorf("verify project workspace references: scan: %w", err)
		}
		broken = append(broken, fmt.Sprintf("%s->%s", projectID, workspaceID))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify project workspace references: %w", err)
	}
	if len(broken) > 0 {
		return fmt.Errorf("project workspace references do not resolve to any workspace row: %s", strings.Join(broken, ", "))
	}
	return nil
}

// migrationStateRow is the (state, input_hash) pair recorded for a
// schema_migrations version.
type migrationStateRow struct {
	state     string
	inputHash string
}

// readMigrationState returns the recorded state/input_hash for version, or
// nil if no row exists yet.
func readMigrationState(conn *sql.DB, version string) (*migrationStateRow, error) {
	var row migrationStateRow
	err := conn.QueryRow(
		`SELECT state, input_hash FROM schema_migrations WHERE version = ?`, version,
	).Scan(&row.state, &row.inputHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// upsertMigrationRow inserts or updates version's schema_migrations row with
// the given state/input_hash, bumping applied_at. Assumes the state/
// input_hash columns already exist.
func upsertMigrationRow(tx *sql.Tx, version, state, inputHash string) error {
	if _, err := tx.Exec(`
		INSERT INTO schema_migrations (version, state, input_hash) VALUES (?, ?, ?)
		ON CONFLICT(version) DO UPDATE SET
			state      = excluded.state,
			input_hash = excluded.input_hash,
			applied_at = datetime('now')
	`, version, state, inputHash); err != nil {
		return fmt.Errorf("record migration %s: %w", version, err)
	}
	return nil
}

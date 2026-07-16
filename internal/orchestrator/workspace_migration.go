package orchestrator

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// workspaceDBConsolidationVersion is the schema_migrations version key used
// by MigrateWorkspaceYAMLToDB's own staging/committed bookkeeping. It is
// tracked in the same schema_migrations table as the file-based migrations
// under internal/db/migrate (migrate.go's `recordMigration`/
// `recordMigrationState`), but recorded directly by this package since this
// migration is Go-side orchestration (yaml/kit parsing, cross-checking
// project references) that a plain .sql file cannot express — see
// docs/plans/workspace-db-consolidation.md マイグレーション節.
const workspaceDBConsolidationVersion = "workspace_db_consolidation"

// MigrateWorkspaceYAMLToDB performs the one-time cutover
// (docs/plans/workspace-db-consolidation.md マイグレーション節 PR3) from
// yaml-file-authority workspaces (DefaultWorkspaceDir()/*.yaml + kit yaml
// under kitsDir) to DB-authority workspaces (the `workspaces` table). Call
// this once at daemon startup, after internal/db/migrate.Apply(conn) has
// already added the schema_migrations.state/input_hash columns (migration
// 0031) and the workspaces table (migration 0030).
//
// Idempotent: once schema_migrations records workspace_db_consolidation as
// state=committed, every subsequent call returns nil immediately without
// touching the filesystem or the DB.
//
// Crash recovery: if a previous call was interrupted between recording
// state=staging and reaching state=committed, the next call recomputes the
// preflight input_hash and compares it against the recorded one — a match
// rolls forward (safe: every step from here on is an idempotent
// upsert/atomic-rename), a mismatch aborts with an error (the on-disk inputs
// changed since the interrupted attempt; automatic reconciliation would risk
// silently mixing old and new state, so this requires manual intervention).
//
// The old workspace yaml files and kitsDir are never modified or deleted by
// this function (decision 16: downgrade-by-restoring-the-prior-binary relies
// on them still being present on disk).
func MigrateWorkspaceYAMLToDB(conn *sql.DB, workspaceDir, kitsDir string, projectRepo *ProjectRepository) error {
	current, err := readMigrationState(conn, workspaceDBConsolidationVersion)
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: read schema_migrations state: %w", err)
	}
	if current != nil && current.state == "committed" {
		return nil
	}

	// Preflight runs with no DB writes at all: every input is parsed and
	// validated (including a fresh project-reference recheck) before we ever
	// touch schema_migrations, so any failure here — corrupt yaml, a kit
	// host_command name collision, a project referencing an unresolvable
	// workspace slug — leaves the database exactly as it was.
	pre, err := preflightWorkspaceMigration(workspaceDir, kitsDir, projectRepo)
	if err != nil {
		return fmt.Errorf("workspace_db_consolidation: preflight: %w", err)
	}

	if current != nil && current.state == "staging" {
		// MAJOR 4 (codex review round 1, docs/plans/workspace-db-consolidation.md
		// Phase 2.5 PR7): a state=staging row recorded by a PR6 binary was
		// hashed with the pre-PR7 shape (no WorkspaceKitRefs field — see
		// computeWorkspaceMigrationInputHash's doc comment). Comparing only
		// against the current (PR7) shape would make every such row a
		// guaranteed mismatch even when the on-disk workspace/kit inputs
		// never changed between the PR6 binary's interrupted attempt and
		// this restart on the PR7 binary — turning a routine binary upgrade
		// into a mandatory "manual intervention" abort. pre.legacyInputHashPR6
		// recomputes the same preflight inputs using that older shape, so an
		// upgrade-in-place with genuinely unchanged inputs still rolls
		// forward; only an actual on-disk change (which changes both hashes)
		// still aborts.
		if current.inputHash != pre.inputHash && current.inputHash != pre.legacyInputHashPR6 {
			return fmt.Errorf(
				"workspace_db_consolidation: found state=staging (input_hash=%q) from an interrupted prior attempt, but the current workspace/kit inputs hash to %q — refusing to roll forward automatically since the on-disk inputs changed since the interruption; restore the prior workspace yaml/kit state (or manually resolve the schema_migrations row) and restart (docs/plans/workspace-db-consolidation.md crash recovery)",
				current.inputHash, pre.inputHash,
			)
		}
		// Recorded input_hash matches what we'd compute right now (either
		// the current PR7 shape, or the legacy PR6 shape from an
		// in-progress binary upgrade): safe to roll forward by re-running
		// the (idempotent) write phase below.
	}

	// Phase 1: record the staging attempt in its own committed transaction,
	// so a crash during phase 2 leaves durable evidence (state=staging) for
	// the next startup's crash-recovery check above. If this were folded
	// into the same transaction as phase 2, an interrupted phase 2 would
	// roll the whole thing back — including the staging marker — and the
	// next startup would see no record of the attempt at all, defeating the
	// point of the two-phase state.
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

	// Phase 2: the actual cutover writes, all inside one transaction (plan
	// step 5 「同一 transaction 内」) committed together with the final
	// state=committed update (plan step 7). A single-process daemon with a
	// single pooled connection (internal/db.Open sets MaxOpenConns(1)) makes
	// a plain Begin() behave like BEGIN IMMEDIATE for our purposes — there is
	// no second connection that could contend for the write lock in between
	// — and decision 11 explicitly waives concurrent-migration races, so no
	// driver-specific BEGIN IMMEDIATE is needed here.
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
	// MAJOR 3 (codex review, 2nd pass): only write the aggregated
	// host_commands.yaml when no file exists there yet, mirroring the same
	// "do not clobber an existing config" guard internal/server/wire.go's
	// buildProjectStore already applies for its own PR2 preflight (the 1st
	// pass only fixed wire.go's side of this). Before this fix,
	// MigrateWorkspaceYAMLToDB called WriteHostCommandsConfig
	// unconditionally on its one committed run, so a PR2-generated or
	// hand-edited config already on disk when this cutover ran was silently
	// replaced by this migration's own freshly-aggregated spec.
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
	// legacyInputHashPR6 (MAJOR 4, codex review round 1) is the same
	// preflight inputs hashed with the pre-Phase-2.5-PR7 shape (no
	// WorkspaceKitRefs field) — see
	// computeWorkspaceMigrationInputHashPR6Shape's doc comment for why
	// MigrateWorkspaceYAMLToDB's crash-recovery check also compares against
	// this.
	legacyInputHashPR6 string
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
	for _, slug := range slugs {
		// MAJOR 5 (codex review round 1, docs/plans/workspace-db-consolidation.md):
		// readWorkspaceYAMLSnapshot reads slug.yaml exactly once and derives
		// both meta and kitRefs from that single byte snapshot — see its doc
		// comment for the TOCTOU this replaces (yamlStore.Load and
		// legacyWorkspaceYAMLKits used to read the same path independently,
		// so an atomic rename racing between the two reads could hand this
		// migration a "meta from the old file version + kits from the new
		// file version" hybrid that never existed on disk at any single
		// instant).
		meta, kitRefs, err := readWorkspaceYAMLSnapshot(resolvedWorkspaceDir, slug)
		if err != nil {
			return nil, fmt.Errorf("read workspace yaml %q: %w", slug, err)
		}
		rawWorkspaces[slug] = meta
		rawKitRefs[slug] = kitRefs
	}

	// MAJOR 1 (codex review): read every installed kit's kit.yaml exactly
	// once into an immutable snapshot, then derive both the aggregated
	// host_commands config and every workspace's kit-materialized runtime
	// (BLOCKER 1, below) from that single snapshot. Before this fix, the
	// aggregate (formerly LoadHostCommandsFromKits) and the per-workspace
	// union (formerly unionKitHostCommandNames) each independently re-read
	// kit.yaml from disk — a kit.yaml edit racing between the two reads
	// could produce an aggregate and a union that silently disagree, and
	// since state=committed makes this preflight run at most once
	// successfully, that inconsistency would be permanent.
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

	// MAJOR 2 (codex review, 2nd pass): pass snap.byKit (every installed
	// kit's raw host_commands/env/additional_bindings) into the hash input
	// too — see computeWorkspaceMigrationInputHash's doc comment for why.
	inputHash, err := computeWorkspaceMigrationInputHash(rawWorkspaces, hostCommands, referenced, snap.byKit, rawKitRefs)
	if err != nil {
		return nil, fmt.Errorf("compute input hash: %w", err)
	}

	// MAJOR 4 (codex review round 1): also hash the very same raw inputs
	// using the pre-Phase-2.5-PR7 shape (no WorkspaceKitRefs field), purely
	// so MigrateWorkspaceYAMLToDB's crash-recovery check can roll forward a
	// state=staging row recorded by a PR6 binary whose on-disk inputs have
	// not actually changed since — see
	// computeWorkspaceMigrationInputHashPR6Shape's doc comment. rawKitRefs
	// (MAJOR 1, codex review round 2) is passed through so each workspace's
	// legacy `kits:` list is restored onto pr6WorkspaceMeta.Kits — without
	// it, this legacy hash could never reflect a workspace's kit references
	// at all, since rawWorkspaces' current WorkspaceMeta no longer has a
	// field to carry them.
	legacyInputHashPR6, err := computeWorkspaceMigrationInputHashPR6Shape(rawWorkspaces, hostCommands, referenced, snap.byKit, rawKitRefs)
	if err != nil {
		return nil, fmt.Errorf("compute legacy (pre-PR7) input hash: %w", err)
	}

	// Build the DB-bound workspace metas: same data as rawWorkspaces, but
	// with HostCommands/Env/AdditionalBindings filled in from each
	// workspace's legacy kit refs (rawKitRefs, read separately above since
	// WorkspaceMeta no longer has a Kits field to carry them — Phase 2.5
	// PR7). Cloned rather than mutated in place so the hash computed above
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
		workspaces:         dbWorkspaces,
		sortedSlugs:        sortedSlugs,
		hostCommands:       hostCommands,
		inputHash:          inputHash,
		legacyInputHashPR6: legacyInputHashPR6,
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
// additional_bindings sections read directly from a single kit.yaml file —
// same reasoning as readKitHostCommandsRaw's doc comment
// (host_commands_config.go), extended to the two other runtime sections
// BLOCKER 1 needs to materialize into a workspace. Used only by
// snapshotAllKitYAMLs.
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
// once and decodes both its WorkspaceMeta fields and its legacy top-level
// `kits:` reference list from that single byte snapshot.
//
// MAJOR 5 (codex review round 1, docs/plans/workspace-db-consolidation.md):
// this replaces what used to be two independent reads of the same path —
// yamlStore.Load(slug) (a full os.ReadFile + yaml.Unmarshal into
// *WorkspaceMeta) followed by the now-removed legacyWorkspaceYAMLKits(dir,
// slug) (a second, separate os.ReadFile + yaml.Unmarshal of the very same
// file, needed only because WorkspaceMeta no longer has a Kits field for the
// first read to populate — Phase 2.5 PR7, decision 12). An atomic rename
// landing between those two reads could hand preflightWorkspaceMigration a
// "meta from the old file version + kits from the new file version" (or vice
// versa) hybrid that never existed on disk at any single instant, which this
// one-time, at-most-once-committed migration would then permanently bake
// into the workspaces table (PR3 already flagged this exact class of TOCTOU
// bug once before, for a different pair of reads). Reading the byte snapshot
// once and decoding both shapes from it makes that hybrid state impossible.
//
// An absent `kits:` key decodes to a nil slice — the fast path, and the
// common case for anything authored post-cutover.
func readWorkspaceYAMLSnapshot(workspaceDir, slug string) (meta *WorkspaceMeta, kitRefs []string, err error) {
	path := filepath.Join(workspaceDir, slug+".yaml")
	raw, err := workspaceYAMLReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	meta = &WorkspaceMeta{}
	if err := yaml.Unmarshal(raw, meta); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var kitsDoc struct {
		Kits []string `yaml:"kits"`
	}
	if err := yaml.Unmarshal(raw, &kitsDoc); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return meta, kitsDoc.Kits, nil
}

// kitYAMLSnapshot is an immutable, once-only read of every installed kit's
// kit.yaml host_commands/env/additional_bindings sections, keyed by kit
// directory name (the kitsDir subdirectory name — same enumeration
// LoadHostCommandsFromKits uses). preflightWorkspaceMigration builds this
// exactly once per call and derives both the aggregated host_commands
// config (aggregateHostCommandsFromSnapshot) and every workspace's
// kit-materialized runtime (materializeKitRuntimeIntoWorkspace) from it —
// the MAJOR 1 fix: before this type existed, the aggregate and the
// per-workspace union each re-read kit.yaml from disk independently, which
// was vulnerable to a kit.yaml edit racing between the two reads.
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
// differing definitions for the same name) but reads from an
// already-taken kitYAMLSnapshot instead of re-scanning kitsDir — the
// MAJOR 1 fix. Kept as a private, migration-only function rather than
// changing LoadHostCommandsFromKits' public signature, since that function
// is also called independently by internal/server/wire.go's own preflight.
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
// host_commands names, Env, and AdditionalBindings into meta (mutated in
// place), reading every kit's data from the already-taken snap rather than
// the filesystem. This is BLOCKER 1's fix: MigrateWorkspaceYAMLToDB
// previously only unioned host_commands *names* from a workspace's Kits
// list (the old unionKitHostCommandNames); the workspaces table has no
// column for Kits (WorkspaceRepository never persists it), so a kit's Env
// and AdditionalBindings were silently dropped the moment the DB became
// authoritative — any dispatch that depended on a workspace kit's env var
// or bind mount would regress after cutover.
//
// Precedence: kit-provided values are defaults, meta's own
// (workspace-authored) values win on conflict — kit-provided values losing
// to workspace-authored ones is a convention this whole mechanism has
// followed since it existed (the retired GetWithWorkspace merge path
// applied the same precedence pre-PR6); this just applies it once here, at
// migration/materialization time, since the materialized result is what
// dispatch reads from this point on.
//
// A kit name with no corresponding entry in snap now aborts the migration
// (MAJOR 2, codex review 3rd pass) rather than warn-and-skip: see this
// function's error return below for why.
//
// Values are taken raw/unexpanded (see readKitRuntimeRaw's doc comment for
// why) — a kit env value or bind-mount path containing a literal ${VAR}
// placeholder is stored as-is. Unlike the migration-time snapshot,
// dispatch-time hydration (ProjectStore.GetWithWorkspace) does expand
// ${VAR} placeholders in the materialized Env/AdditionalBindings — see
// expandWorkspaceRuntimeForDispatch (workspace_meta.go, MAJOR 1 codex review
// 3rd pass).
func materializeKitRuntimeIntoWorkspace(snap *kitYAMLSnapshot, kits []string, meta *WorkspaceMeta) error {
	if len(kits) == 0 {
		return nil
	}

	seenHostCommandNames := make(map[string]struct{})
	var kitHostCommandNames []string
	kitEnv := make(map[string]string)
	var kitBindings []BindMount
	// MAJOR 3 (codex review, 3rd pass): kit root bindings are collected in
	// their own slice, kept out of kitBindings entirely — see the comment
	// below, at the point they are appended to meta.AdditionalBindings, for
	// why.
	var kitRootBindings []BindMount
	seenKitRoots := make(map[string]struct{})

	for _, kitName := range kits {
		raw, ok := snap.byKit[kitName]
		if !ok {
			// MAJOR 2 (codex review, 3rd pass): abort instead of
			// warn-and-skip. MigrateWorkspaceYAMLToDB commits at most once
			// (state=committed makes every later call a no-op) — if a kit
			// directory is merely temporarily missing/not-yet-mounted at
			// the exact moment the daemon happens to run this one-time
			// cutover, warn-and-skip would permanently strand this
			// workspace's kit-supplied env/host_commands/bindings with no
			// way to recover other than hand-editing the workspaces table,
			// since materializeKitRuntimeIntoWorkspace (and the Kits list
			// itself) never runs again afterward. Failing preflight instead
			// leaves zero DB changes (preflightWorkspaceMigration performs
			// no writes), so the operator can restore the kit directory —
			// or remove the reference from the workspace's kits list — and
			// simply restart the daemon.
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
		kitBindings = unionBindMountSlices(kitBindings, raw.AdditionalBindings)

		// BLOCKER (codex review, 2nd pass): also materialize the kit
		// directory itself as a read-only bind mount, mirroring the
		// KitRoots mechanism (MergeKitMetaIntoBehavior in spec_loader.go /
		// sandbox_builder.go's spec.Visibility.KitRoots) that shell hooks
		// rely on to read kit-dir scripts/assets. The workspaces table has
		// no column for Kits at all, so once cutover commits, KitRoots
		// itself is never regenerated again for this workspace — this bind
		// mount, folded into meta.AdditionalBindings, is what carries the
		// "expose this kit's directory" guarantee across the DB boundary
		// from now on (picked up at dispatch time by GetWithWorkspace's
		// existing ws.AdditionalBindings merge block, BLOCKER 2).
		//
		// MAJOR 3 (codex review, 3rd pass): kept in the dedicated
		// kitRootBindings slice rather than folded into kitBindings via
		// unionBindMountSlices, as the 2nd-pass fix originally did.
		// unionBindMountSlices and mergeBindMounts both key solely on
		// Source; on a Source collision the former silently swallows the
		// new entry and the latter wholesale-replaces the existing one —
		// either way this kit-root mount could vanish if any other
		// binding (the kit's own additional_bindings, or a
		// workspace-authored one) happens to share the kit directory as
		// Source for an unrelated purpose. The old, independent KitRoots
		// mechanism (behavior.KitRoots) never had this failure mode since
		// it was never merged into AdditionalBindings at all; appending
		// kitRootBindings on its own (below, after the ordinary
		// kitBindings merge) restores that independence without a new
		// column, at the cost of a possible duplicate Source in the final
		// list — harmless, since dispatch mounts each entry independently.
		//
		// Target is left empty (implicit, resolved by
		// additionalBindingMounts to Source), Mode is left empty (default
		// read-only), matching the original KitRoots contract that kit
		// scripts/assets are never writable from inside the sandbox
		// (codex review, 4th pass — an earlier iteration used explicit
		// Target + Mode="rw" out of a misreading of the self-mount guard
		// in dispatcher/sandbox_builder.go: the guard is `explicitTarget
		// && Source==Target && Mode != "rw"`, and `explicitTarget` is
		// `bm.Target != ""` — so an *empty* Target already skips the guard
		// entirely without needing rw, and the resulting mount is still
		// pinned to Source==Target by additionalBindingMounts's default).
		// Dedupe safety is preserved because kitRootBindings is appended
		// unconditionally below, bypassing every Source-keyed merge path.
		kitDir := filepath.Join(snap.kitsDir, kitName)
		if _, dup := seenKitRoots[kitDir]; !dup {
			seenKitRoots[kitDir] = struct{}{}
			kitRootBindings = append(kitRootBindings, BindMount{Source: kitDir})
		}
	}

	sort.Strings(kitHostCommandNames)
	meta.HostCommands = unionStringsSorted(meta.HostCommands, kitHostCommandNames)

	if len(kitEnv) > 0 {
		// meta.Env (workspace-authored) wins over kit-supplied defaults.
		meta.Env = mergeStringMaps(kitEnv, meta.Env)
	}

	if len(kitBindings) > 0 {
		// MAJOR 1 (codex review, 2nd pass): fold kitBindings into meta via
		// mergeBindMounts(base=kit, overlay=workspace-authored), not
		// unionBindMountSlices. unionBindMountSlices only ever *promotes* a
		// Source's Mode to "rw" on a collision — it can never demote a
		// kit's rw binding back down to the workspace's own explicit ro for
		// the same Source, which inverted the intended "workspace-authored
		// wins over kit-supplied default" precedence (a workspace's
		// explicit ro would be silently promoted to rw merely because a
		// kit happened to declare the same Source as rw).
		// mergeBindMounts(base, overlay) instead has the overlay entry
		// replace the base entry outright on a Source match, so
		// meta.AdditionalBindings — already populated from the raw,
		// workspace-authored yaml by the time this function runs, see
		// cloneWorkspaceMetaForMigration's caller in
		// preflightWorkspaceMigration — wins whole-hog, matching the
		// precedence documented above this function.
		meta.AdditionalBindings = mergeBindMounts(kitBindings, meta.AdditionalBindings)
	}

	// MAJOR 3 (codex review, 3rd pass): kit root bindings bypass the
	// Source-keyed merge above entirely (see the comment where they are
	// collected) and are appended unconditionally, so each one always
	// reaches dispatch regardless of what else shares its Source.
	if len(kitRootBindings) > 0 {
		meta.AdditionalBindings = append(meta.AdditionalBindings, kitRootBindings...)
	}

	return nil
}

// MaterializeWorkspaceKitsForPersist resolves kitRefs (a legacy `kits:`
// reference list, sourced by the caller — see below) against the kits
// installed under kitsDir, merging their host_commands (folded in as
// reference names)/env/additional_bindings into meta in place — the exact
// same expansion MigrateWorkspaceYAMLToDB performs once at cutover.
//
// Phase 2.5 PR7 (docs/plans/workspace-db-consolidation.md, decision 12)
// removed WorkspaceMeta.Kits outright: this function used to read
// meta.Kits and clear it after resolving. There is no longer a Kits field
// to read, so kitRefs is now an explicit parameter the caller must source
// itself. The only remaining caller is cmd/workspace.go's
// ensureWorkspaceExistsForAssign (`boid workspace assign`'s auto-create
// convenience path), which extracts a legacy `kits:` key straight out of
// the raw on-disk shadow yaml before this call — the wire-level
// POST/PUT/import bodies no longer accept a kits: key at all (workspaceMetaStrict
// has no such field any more), so no server-side caller needs this any
// longer. cmd/project_migrate.go no longer calls this either: its own
// auto-generated legacy kit's host_commands/additional_bindings are folded
// directly into the workspace meta from the legacy project.yaml's own
// fields (mergeLegacyFieldsIntoWorkspace), with no kit-directory round trip
// needed at all — only a *pre-existing, externally referenced* kit (like
// ensureWorkspaceExistsForAssign's case) needs this function's disk lookup.
//
// This was discovered as a real e2e regression (docker-proxy-* scenarios
// failing with "$DOCKER_PROXY_TEST_ROOT/docker-proxy-test.sh: not found",
// exit 127) when `boid workspace assign`'s auto-create path (introduced in
// PR4) funneled a legacy `kits: [docker-proxy-test]` yaml straight into
// WorkspaceRepository.Create without this expansion step — the workspaces
// table has no kits column at all (decision 「kits カラム無し」), so an
// unmaterialized kit reference would silently vanish on save.
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
	// KitRuntime (MAJOR 2, codex review 2nd pass) is every installed kit's
	// raw host_commands/env/additional_bindings snapshot (kitYAMLSnapshot.
	// byKit), keyed by kit dir name. Before this field existed, a kit.yaml
	// edit that changed only its env or additional_bindings section (not
	// its host_commands, and not any workspace yaml) went completely
	// unnoticed by the hash: HostCommands above only carries the aggregated
	// host_commands names/specs, and Workspaces above carries the raw
	// (pre-materialization) workspace metas, which never held kit env/
	// bindings data in the first place — that only gets folded in by
	// materializeKitRuntimeIntoWorkspace, which runs *after* the hash is
	// computed. So a kit env/binding-only edit racing between a staged and
	// a resumed migration attempt (MigrateWorkspaceYAMLToDB's crash
	// recovery) would compute the same input_hash before and after,
	// silently rolling forward with the changed values instead of aborting
	// per the documented crash-recovery contract.
	KitRuntime map[string]kitRuntimeRaw `json:"kit_runtime"`
	// WorkspaceKitRefs (Phase 2.5 PR7, docs/plans/workspace-db-consolidation.md
	// decision 12) is each workspace's legacy `kits:` reference list, read
	// directly off the raw yaml file (readWorkspaceYAMLSnapshot) since
	// WorkspaceMeta no longer has a Kits field for Workspaces above to
	// carry. Without this, editing only a workspace yaml's `kits:` list
	// (adding/removing/reordering a reference, with no other field
	// changing) between a staged and a resumed migration attempt would go
	// completely undetected by the hash — the same class of bug KitRuntime
	// above fixes for a referenced kit's own content.
	WorkspaceKitRefs map[string][]string `json:"workspace_kit_refs"`
}

// computeWorkspaceMigrationInputHash hashes the raw (pre-union) workspace
// metas, the aggregated kit host_commands, the project->workspace reference
// list, every installed kit's raw runtime snapshot (env/additional_bindings
// included, MAJOR 2), and each workspace's legacy kit ref list (Phase 2.5
// PR7) — everything preflightWorkspaceMigration consulted — into a single
// sha256 hex digest, used by MigrateWorkspaceYAMLToDB's crash recovery to
// detect whether the on-disk/DB inputs changed since an interrupted
// attempt.
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
// tag-for-tag, order-for-order, exactly as it existed in Phase 2.5 PR6 (git
// commit fb1f222, "feat: Phase 2.5 PR6 (kit 機構退役)") — before PR7 (decision
// 12) removed the Kits field from WorkspaceMeta outright. Used ONLY by
// computeWorkspaceMigrationInputHashPR6Shape for legacy hash reconstruction
// (MAJOR 4, codex review round 1). Go's encoding/json marshals struct fields
// in declaration order (unlike map keys, which it sorts), so this field
// order is not cosmetic — it is exactly what makes json.Marshal of this type
// byte-identical to what a PR6 binary produced for the same logical data.
//
// IMPORTANT: do NOT modify this struct once PR7 lands — including to mirror
// a future field WorkspaceMeta gains, or to "clean up" the Kits field back
// into WorkspaceMeta's own now-different position. Its byte shape must stay
// stable forever to keep the crash-recovery upgrade path
// (computeWorkspaceMigrationInputHashPR6Shape / MigrateWorkspaceYAMLToDB)
// deterministic for any state=staging row a PR6 binary may have left on disk.
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
// as it existed before Phase 2.5 PR7 added the WorkspaceKitRefs field
// (decision 12) — used only by computeWorkspaceMigrationInputHashPR6Shape,
// itself used only for MigrateWorkspaceYAMLToDB's crash-recovery upgrade
// check (MAJOR 4, codex review round 1). IMPORTANT: do NOT add
// WorkspaceKitRefs (or any other field workspaceMigrationHashInput gains in
// the future) here — the whole point of this type is to keep reproducing
// the exact byte shape a PR6 binary would have hashed, forever.
//
// Workspaces is keyed to pr6WorkspaceMeta rather than the current (PR7)
// WorkspaceMeta (MAJOR 1, codex review round 2): PR6's WorkspaceMeta carried
// a Kits field directly, so PR6's own computeWorkspaceMigrationInputHash
// hashed each workspace's kit references as part of its Workspaces entry.
// Reusing the current, Kits-less WorkspaceMeta here can never reproduce
// that — it has no field to carry the value at all — so every workspace
// that referenced a kit would silently hash differently from what a real
// PR6 binary computed for the identical on-disk inputs, defeating MAJOR 4's
// crash-recovery upgrade check specifically for the workspaces it matters
// most for (the ones with kit-supplied host_commands/env/bindings to lose).
type workspaceMigrationHashInputPR6 struct {
	Workspaces           map[string]*pr6WorkspaceMeta `json:"workspaces"`
	HostCommands         map[string]HostCommandSpec   `json:"host_commands"`
	ProjectWorkspaceRefs []*WorkspaceSummary          `json:"project_workspace_refs"`
	KitRuntime           map[string]kitRuntimeRaw     `json:"kit_runtime"`
}

// computeWorkspaceMigrationInputHashPR6Shape recomputes
// preflightWorkspaceMigration's input hash using the pre-Phase-2.5-PR7 shape
// (no WorkspaceKitRefs field, and each workspace rehydrated with its Kits
// field restored from workspaceKitRefs) from the very same raw inputs
// computeWorkspaceMigrationInputHash was given (MAJOR 4, codex review round
// 1; MAJOR 1, codex review round 2, docs/plans/workspace-db-consolidation.md).
//
// Why this exists: PR7 added a 5th field (WorkspaceKitRefs) to
// workspaceMigrationHashInput to close a real hash-blind-spot (a workspace
// yaml's `kits:` list changing undetected — see that field's own doc
// comment). But MigrateWorkspaceYAMLToDB's crash recovery persists
// input_hash across a daemon binary upgrade: a PR6 binary that recorded
// state=staging (interrupted mid-migration) computed its input_hash with
// the *old* 4-field shape, where each workspace's own WorkspaceMeta.Kits
// field carried its kit references directly. Restarting on a PR7 binary
// recomputes the hash with the new 5-field shape unconditionally — which,
// for every possible on-disk input, differs from whatever a PR6 binary
// would have recorded, even when nothing on disk actually changed between
// the interrupted attempt and this restart. Without this fallback, every
// such upgrade would hit the crash-recovery "inputs changed, refusing to
// roll forward automatically" abort and demand manual intervention, even
// though the abort's entire premise (the inputs actually changed) is false.
// Comparing the recorded hash against *both* shapes lets a genuine
// upgrade-in-place roll forward while still aborting on an actual on-disk
// change (which changes both shapes' hashes alike) — but only if this
// shape's Workspaces entries actually carry Kits (MAJOR 1's fix); workspaceKitRefs
// (readWorkspaceYAMLSnapshot's per-slug legacy `kits:` list, the same map
// computeWorkspaceMigrationInputHash's own WorkspaceKitRefs field is built
// from) is what restores that value here, since workspaces itself (the
// current, PR7-shaped WorkspaceMeta) no longer carries it.
func computeWorkspaceMigrationInputHashPR6Shape(
	workspaces map[string]*WorkspaceMeta,
	hostCommands map[string]HostCommandSpec,
	projectRefs []*WorkspaceSummary,
	kitRuntime map[string]kitRuntimeRaw,
	workspaceKitRefs map[string][]string,
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
			AdditionalBindings: meta.AdditionalBindings,
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

// verifyProjectWorkspaceRefsResolvable re-checks, from inside the write
// transaction, that every project_workspaces.workspace_id resolves to a row
// now present in workspaces (plan step 5's third bullet). This duplicates
// preflightWorkspaceMigration's check deliberately: that first check ran
// before any workspace row existed in the DB (comparing against parsed yaml
// slugs instead), so this is the check that actually matters for
// correctness — it runs after every workspace has been written and the
// default workspace ensured, inside the same transaction, so a stale read
// from outside the transaction is not possible.
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
// input_hash columns already exist (added by internal/db/migrate migration
// 0031, which MigrateWorkspaceYAMLToDB's caller runs before calling this
// function — see wire.go's buildProjectStore).
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

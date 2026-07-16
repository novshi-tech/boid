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
		if current.inputHash != pre.inputHash {
			return fmt.Errorf(
				"workspace_db_consolidation: found state=staging (input_hash=%q) from an interrupted prior attempt, but the current workspace/kit inputs hash to %q — refusing to roll forward automatically since the on-disk inputs changed since the interruption; restore the prior workspace yaml/kit state (or manually resolve the schema_migrations row) and restart (docs/plans/workspace-db-consolidation.md crash recovery)",
				current.inputHash, pre.inputHash,
			)
		}
		// Recorded input_hash matches what we'd compute right now: safe to
		// roll forward by re-running the (idempotent) write phase below.
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

	rawWorkspaces := make(map[string]*WorkspaceMeta, len(slugs))
	for _, slug := range slugs {
		meta, err := yamlStore.Load(slug)
		if err != nil {
			return nil, fmt.Errorf("parse workspace yaml %q: %w", slug, err)
		}
		rawWorkspaces[slug] = meta
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
	inputHash, err := computeWorkspaceMigrationInputHash(rawWorkspaces, hostCommands, referenced, snap.byKit)
	if err != nil {
		return nil, fmt.Errorf("compute input hash: %w", err)
	}

	// Build the DB-bound workspace metas: same data as rawWorkspaces, but
	// with HostCommands/Env/AdditionalBindings filled in from each
	// workspace's Kits (plan note: 「workspace.HostCommands は [kits[].
	// host_commands の名前を全部 flatten して重複除去した結果] を fill」,
	// extended by BLOCKER 1 below to Env and AdditionalBindings). Cloned
	// rather than mutated in place so the hash computed above reflects only
	// the raw, unexpanded yaml/kit inputs.
	dbWorkspaces := make(map[string]*WorkspaceMeta, len(rawWorkspaces))
	for slug, raw := range rawWorkspaces {
		meta := cloneWorkspaceMetaForMigration(raw)
		if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
			return nil, fmt.Errorf("workspace %q: materialize kit runtime: %w", slug, err)
		}
		// BLOCKER 1 (codex review): the workspaces table has no column for
		// Kits (WorkspaceRepository.Load/Save never reads/writes it), so
		// once this migration commits, a DB-backed WorkspaceMeta always
		// comes back with Kits == nil. materializeKitRuntimeIntoWorkspace
		// just folded every named kit's host_commands/env/
		// additional_bindings into meta's own fields, so clearing Kits here
		// mirrors what a DB round-trip will do anyway — and, as a direct
		// consequence, GetWithWorkspace's separate ws.Kits merge block
		// (which re-resolves and re-reads kit.yaml at every hydration) is
		// now dead code on the committed/DB-backed path: it only still
		// fires for the legacy yaml-mode WorkspaceStore (repo == nil).
		meta.Kits = nil
		dbWorkspaces[slug] = meta
	}

	sortedSlugs := make([]string, 0, len(dbWorkspaces))
	for slug := range dbWorkspaces {
		sortedSlugs = append(sortedSlugs, slug)
	}
	sort.Strings(sortedSlugs)

	return &workspaceMigrationPreflight{
		workspaces:   dbWorkspaces,
		sortedSlugs:  sortedSlugs,
		hostCommands: hostCommands,
		inputHash:    inputHash,
	}, nil
}

// cloneWorkspaceMetaForMigration returns a shallow copy of meta.
// HostCommands, Env, and AdditionalBindings are the only fields ever
// reassigned (never appended to in place) on the clone by
// preflightWorkspaceMigration's caller and by materializeKitRuntimeIntoWorkspace
// (each merge helper they call — unionStringsSorted / mergeStringMaps /
// unionBindMountSlices — returns a brand-new slice/map rather than mutating
// its input), so a shallow copy — which leaves every field's slice/map
// initially sharing the original's backing array — is safe: nothing is ever
// mutated in place through the clone.
func cloneWorkspaceMetaForMigration(meta *WorkspaceMeta) *WorkspaceMeta {
	clone := *meta
	return &clone
}

// kitRuntimeRaw holds the raw (unexpanded) host_commands / env /
// additional_bindings sections read directly from a single kit.yaml file,
// bypassing ReadKitMeta's validation/interpolation pipeline — same
// reasoning as readKitHostCommandsRaw's doc comment (host_commands_config.go),
// extended to the two other runtime sections BLOCKER 1 needs to materialize
// into a workspace. Used only by snapshotAllKitYAMLs; the per-request
// ws.Kits merge path in GetWithWorkspace still uses the fully validated
// ReadKitMeta.
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
// (workspace-authored) values win on conflict — the same "kit loses to
// workspace" precedence GetWithWorkspace already applies for the
// yaml-mode/ws.Kits path at every hydration; this just applies it once here,
// at migration time, since the materialized result is what dispatch reads
// from this point on.
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
		// no column for Kits at all (see the `meta.Kits = nil` comment in
		// preflightWorkspaceMigration), so once cutover commits, KitRoots
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

// MaterializeWorkspaceKitsForPersist resolves meta.Kits (if any) against the
// kits installed under kitsDir, merges their host_commands (folded in as
// reference names)/env/additional_bindings into meta in place — the exact
// same expansion MigrateWorkspaceYAMLToDB performs once at cutover — and
// then clears meta.Kits.
//
// Call this before persisting any *WorkspaceMeta that might still carry a
// legacy Kits list through WorkspaceRepository.Create/Save (docs/plans/
// workspace-db-consolidation.md PR4: POST/PUT /api/workspaces, and by
// extension `boid workspace create`/`edit` and `assign`'s legacy-yaml
// auto-create). The workspaces table has no kits column at all (decision
// 「kits カラム無し」), so a Kits value left unmaterialized would silently
// vanish on save regardless — and once gone, it can never be re-resolved:
// GetWithWorkspace's own per-hydration ws.Kits merge block only ever sees
// whatever ended up (persisted) in the DB row, which would be empty. This
// was discovered as a real e2e regression (docker-proxy-* scenarios failing
// with "$DOCKER_PROXY_TEST_ROOT/docker-proxy-test.sh: not found", exit 127)
// when `boid workspace assign`'s auto-create path (introduced in PR4)
// funneled a legacy `kits: [docker-proxy-test]` yaml straight into
// WorkspaceRepository.Create without this expansion step.
//
// meta.Kits == nil/empty is a fast path: the overwhelming majority of
// calls (any workspace that has never referenced a kit) never touch the
// filesystem at all.
func MaterializeWorkspaceKitsForPersist(kitsDir string, meta *WorkspaceMeta) error {
	if meta == nil || len(meta.Kits) == 0 {
		return nil
	}
	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		return fmt.Errorf("snapshot kit yaml: %w", err)
	}
	if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
		return err
	}
	meta.Kits = nil
	return nil
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
}

// computeWorkspaceMigrationInputHash hashes the raw (pre-union) workspace
// metas, the aggregated kit host_commands, the project->workspace reference
// list, and every installed kit's raw runtime snapshot (env/
// additional_bindings included, MAJOR 2) — everything
// preflightWorkspaceMigration consulted — into a single sha256 hex digest,
// used by MigrateWorkspaceYAMLToDB's crash recovery to detect whether the
// on-disk/DB inputs changed since an interrupted attempt.
func computeWorkspaceMigrationInputHash(
	workspaces map[string]*WorkspaceMeta,
	hostCommands map[string]HostCommandSpec,
	projectRefs []*WorkspaceSummary,
	kitRuntime map[string]kitRuntimeRaw,
) (string, error) {
	sortedRefs := append([]*WorkspaceSummary(nil), projectRefs...)
	sort.Slice(sortedRefs, func(i, j int) bool { return sortedRefs[i].ID < sortedRefs[j].ID })

	b, err := json.Marshal(workspaceMigrationHashInput{
		Workspaces:           workspaces,
		HostCommands:         hostCommands,
		ProjectWorkspaceRefs: sortedRefs,
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

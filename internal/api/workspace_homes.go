package api

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/humanize"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// WorkspaceHomeSize describes one workspace home directory's on-disk
// footprint (docs/plans/home-workspace-volume.md Phase 4 PR5): used both by
// GET /api/workspaces/{slug} (a single entry, WorkspaceDetail.Home) and by
// POST /api/gc's workspace_homes listing (one entry per directory found
// under homes/).
type WorkspaceHomeSize struct {
	Slug string `json:"slug"`
	// Path is the absolute on-disk path to the home directory, whether or
	// not it currently exists.
	Path string `json:"path"`
	// Exists reports whether Path exists on disk. false means the workspace
	// has never been dispatched into (docs/plans/home-workspace-volume.md's
	// init-on-first-dispatch contract) — not an error.
	Exists bool `json:"exists"`
	// Bytes is the recursive apparent size of Path (humanize.ApparentSize —
	// not a `du`-equivalent block-based size, see that function's doc
	// comment), meaningful only when Exists is true and SizeError is empty.
	Bytes int64 `json:"bytes"`
	// Orphan is true when Path was found on disk but Slug has no
	// corresponding workspace row — a workspace.yaml/DB row that was
	// removed (or never assign/create'd) while its home directory survived.
	// Only ever set by the GC listing (ListWorkspaceHomeSizes); the single-
	// slug lookup used by GET /api/workspaces/{slug} always has a live
	// workspace row (it is 404 otherwise), so it never carries Orphan=true.
	Orphan bool `json:"orphan"`
	// SizeError is non-empty when humanize.ApparentSize (or stat-ing Path in
	// the first place) failed for a reason other than "does not exist" —
	// typically a permission error, or a concurrent dispatch mutating the
	// tree mid-walk. Bytes is 0 and must not be trusted when this is set;
	// callers render "?" instead (docs/plans/home-workspace-volume.md PR5:
	// "エラー時はエラーにせず「?」表示"). A non-empty SizeError does *not*
	// imply the directory itself is gone or undeletable — deleteWorkspaceHome
	// treats sizing as best-effort and still attempts deletion regardless
	// (codex PR #791 review, Should-fix #2).
	SizeError string `json:"size_error,omitempty"`
}

// WorkspaceSlugLister exposes the set of currently known workspace slugs,
// used to flag orphaned home directories (a directory under homes/ with no
// corresponding workspace row). *ProjectAppService already satisfies this
// via its existing ListWorkspaces method — no new implementation is needed,
// only threading it through as this narrower interface at the handler
// construction site (server/wire.go) so GCHandler does not need the whole
// ProjectService surface.
type WorkspaceSlugLister interface {
	ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error)
}

// resolveWorkspaceHomePath returns the absolute on-disk path for slug's
// workspace home directory, derived from runtimesDir via
// dispatcher.WorkspaceHomesDir — the exact same computation the dispatcher
// itself uses to mount a job's $HOME (docs/plans/home-workspace-volume.md
// Phase 4 PR2), so a size reported here always matches what is actually
// mounted. Does not check whether the path exists.
func resolveWorkspaceHomePath(runtimesDir, slug string) (string, error) {
	homesDir, err := dispatcher.WorkspaceHomesDir(runtimesDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(homesDir, slug), nil
}

// apparentSizeFn is a package-level indirection to humanize.ApparentSize,
// swappable in tests (mirrors internal/orchestrator/workspace_migration.go's
// workspaceYAMLReadFile and internal/sandbox/fetch_builtin.go's
// newFetchClient) — lets a test inject a sizing failure deterministically
// without needing a real filesystem race or a permission trick that would
// also block the RemoveAll deleteWorkspaceHome attempts alongside it (codex
// PR #791 review, Should-fix #2: "サイズ計算失敗しても削除は成功する" needs a
// sizing failure that does *not* also block deletion, which real permission
// bits cannot isolate on their own).
var apparentSizeFn = humanize.ApparentSize

// computeWorkspaceHomeSize resolves slug's home directory path and reports
// its on-disk size, never returning an error itself: every failure mode
// (unresolvable runtimesDir, the directory not existing yet, a permission
// error walking it) is instead reflected in the returned WorkspaceHomeSize's
// Exists/SizeError fields, matching the "never block the rest of the
// response on a size lookup failure" contract PR5's brief sets for both
// `workspace show` and `boid gc`.
func computeWorkspaceHomeSize(runtimesDir, slug string) WorkspaceHomeSize {
	entry := WorkspaceHomeSize{Slug: slug}

	path, err := resolveWorkspaceHomePath(runtimesDir, slug)
	if err != nil {
		entry.SizeError = err.Error()
		return entry
	}
	entry.Path = path

	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return entry // Exists stays false; not an error.
		}
		entry.SizeError = statErr.Error()
		return entry
	}
	entry.Exists = true

	n, sizeErr := apparentSizeFn(path)
	if sizeErr != nil {
		entry.SizeError = sizeErr.Error()
		return entry
	}
	entry.Bytes = n
	return entry
}

// ListWorkspaceHomeSizes enumerates every directory under runtimesDir's
// sibling homes/ directory (docs/plans/home-workspace-volume.md's layout:
// homes/<slug>/, alongside sibling homes/<slug>.init.json and
// homes/<slug>.lock files that os.ReadDir's IsDir() filter excludes), and
// reports each one's size. Unlike computeWorkspaceHomeSize's single-slug
// lookup (driven by an already-known, already-validated slug), this walks
// the directory itself — so it also surfaces home directories with no
// corresponding workspace row at all ("orphans": docs/plans/
// home-workspace-volume.md PR5: "workspace.yaml が消えて home だけ残った
// 孤児はレポートのみ"), which lister (when non-nil) is consulted to detect.
//
// Orphan detection depends on a successful WorkspaceSlugLister.List call. A
// lister failure means orphan status is simply unknowable for every entry —
// rather than marking every entry Orphan=true (this function's pre-#791-
// review behavior, which silently misreported every real workspace's home
// as an "orphan" on a merely transient DB hiccup), a lister failure now
// omits the listing outright: homes comes back an empty (non-nil) slice, and
// listErr carries the lister's error message so a caller can render "size
// listing unavailable: <reason>" instead of trusting bogus per-entry orphan
// flags. This preserves the invariant "every WorkspaceHomeSize actually
// returned has a trustworthy Orphan flag" (codex PR #791 review, Should-fix
// #3, selection A — the plan's rejected alternative, selection B, would
// instead add a 3-state `orphan_known bool` per entry).
//
// err (the third return) is reserved for a genuine directory-walk failure —
// an unresolvable runtimesDir, or os.ReadDir failing for a reason other than
// "does not exist yet". Unlike listErr, a non-nil err means the whole call
// failed, not just orphan detection; GC's own record-deletion work does not
// depend on workspace-home reporting succeeding, so callers should still
// treat a non-nil err as non-fatal to the rest of their own response.
//
// Returns an empty (non-nil) slice, not an error, when homes/ does not exist
// yet (a fresh installation that has never dispatched a job).
func ListWorkspaceHomeSizes(runtimesDir string, lister WorkspaceSlugLister) (homes []WorkspaceHomeSize, listErr string, err error) {
	homesDir, err := dispatcher.WorkspaceHomesDir(runtimesDir)
	if err != nil {
		return nil, "", err
	}

	entries, err := os.ReadDir(homesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []WorkspaceHomeSize{}, "", nil
		}
		return nil, "", err
	}

	known := map[string]bool{}
	if lister != nil {
		summaries, lerr := lister.ListWorkspaces()
		if lerr != nil {
			slog.Warn("list workspace homes: ListWorkspaces failed, omitting workspace_homes listing", "error", lerr)
			return []WorkspaceHomeSize{}, lerr.Error(), nil
		}
		for _, s := range summaries {
			known[s.ID] = true
		}
	}

	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			slugs = append(slugs, e.Name())
		}
	}
	sort.Strings(slugs)

	result := make([]WorkspaceHomeSize, 0, len(slugs))
	for _, slug := range slugs {
		entry := computeWorkspaceHomeSize(runtimesDir, slug)
		entry.Orphan = !known[slug]
		result = append(result, entry)
	}
	return result, "", nil
}

// WorkspaceRemoveResponse is the response body for DELETE
// /api/workspaces/{slug} (docs/plans/home-workspace-volume.md Phase 4 PR5).
// The workspace row is always removed before any home-directory deletion is
// attempted (WorkspaceHandler.Remove calls Service.RemoveWorkspace first),
// so a non-empty HomeDeleteError (or HomeSizeError) reports a *partially*
// completed remove — deliberately allowed per the plan doc ("削除失敗...
// workspace 設定 (DB) の削除は先に完了させる (part-completed 状態を許容...)")
// rather than treated as a request failure: the response status stays 200.
type WorkspaceRemoveResponse struct {
	Status string `json:"status"`
	// HomePath/HomeBytes describe the home directory as it was found right
	// before the deletion attempt (empty/0 when RuntimesDir was not wired
	// into the handler, or slug is the reserved default workspace). HomeBytes
	// is only trustworthy when HomeSizeError is empty.
	HomePath  string `json:"home_path,omitempty"`
	HomeBytes int64  `json:"home_bytes,omitempty"`
	// HomeSizeError is non-empty when the daemon could not determine
	// HomePath's on-disk size before attempting deletion (mirrors
	// WorkspaceHomeSize.SizeError) — independent of whether the deletion
	// itself subsequently succeeded. Split out from HomeDeleteError (codex
	// PR #791 review, Should-fix #2): the two used to be conflated into a
	// single field, so a caller could not tell "we don't know the size" (a
	// diagnostic-only hiccup, e.g. a concurrent dispatch mutating the tree
	// mid-walk) apart from "the directory is still there and undeletable" (a
	// real part-completed-remove problem worth investigating).
	HomeSizeError string `json:"home_size_error,omitempty"`
	// HomeDeleted is true only when a home directory existed and os.RemoveAll
	// on it succeeded. false covers every other case: no RuntimesDir wired,
	// the default workspace's protected home, no home directory to begin
	// with, or a deletion failure (see HomeDeleteError for that last one).
	// Sizing is best-effort (see HomeSizeError) and never gates whether
	// deletion is attempted — HomeDeleted can be true even when
	// HomeSizeError is also non-empty.
	HomeDeleted bool `json:"home_deleted"`
	// HomeDeleteError is non-empty only when os.RemoveAll on the home
	// directory itself failed. Never populated merely because sizing failed
	// (see HomeSizeError for that).
	HomeDeleteError string `json:"home_delete_error,omitempty"`
}

// deleteWorkspaceHome removes slug's workspace home directory (docs/plans/
// home-workspace-volume.md Phase 4 PR5, DELETE /api/workspaces/{slug}).
//
// The reserved default workspace is refused unconditionally here as defense
// in depth: ProjectAppService.RemoveWorkspace already rejects removing the
// default workspace's *row* before WorkspaceHandler.Remove ever calls this
// function, so in practice this branch should be unreachable — but home
// directory deletion gets its own independent guard anyway, so a future
// caller of this function (or a bug in the row-level guard) cannot
// accidentally destroy the default workspace's persistent $HOME.
//
// Sizing is best-effort (codex PR #791 review, Should-fix #1/#2): the
// pre-#791-review version bailed out here whenever info.SizeError was set —
// including the common case where computeWorkspaceHomeSize's top-level
// os.Stat succeeded (info.Exists is true — something is really there) but
// the subsequent humanize.ApparentSize walk failed partway through, e.g.
// because a concurrent dispatch job deleted cache files out from under it.
// That meant a mere sizing hiccup silently skipped os.RemoveAll entirely,
// leaving an undeleted directory behind after the workspace row itself was
// already gone (an orphan). RemoveAll is now attempted whenever anything
// might be there — info.Exists is true, or info.Path resolved to something
// stat could not conclusively rule out (a permission error, not ENOENT) — so
// a sizing failure never blocks the delete attempt; only info.Path being
// empty (runtimesDir itself unresolvable) skips it outright, since there is
// then no path to call RemoveAll on at all.
func deleteWorkspaceHome(runtimesDir, slug string) (info WorkspaceHomeSize, deleted bool, err error) {
	info = computeWorkspaceHomeSize(runtimesDir, slug)

	if slug == orchestrator.DefaultWorkspaceSlug {
		return info, false, nil
	}
	if info.Path == "" {
		// runtimesDir itself could not be resolved (info.SizeError already
		// explains why) — there is no path to attempt RemoveAll on.
		return info, false, nil
	}

	if err := os.RemoveAll(info.Path); err != nil {
		return info, false, err
	}
	// os.RemoveAll succeeds (nil error) even when info.Path never existed in
	// the first place — report deleted=true only when something was
	// actually found there beforehand (info.Exists), not merely because the
	// no-op RemoveAll call itself didn't error.
	return info, info.Exists, nil
}

package api

import (
	"errors"
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
	// Bytes is the recursive size of Path (humanize.DirSize), meaningful
	// only when Exists is true and SizeError is empty.
	Bytes int64 `json:"bytes"`
	// Orphan is true when Path was found on disk but Slug has no
	// corresponding workspace row — a workspace.yaml/DB row that was
	// removed (or never assign/create'd) while its home directory survived.
	// Only ever set by the GC listing (ListWorkspaceHomeSizes); the single-
	// slug lookup used by GET /api/workspaces/{slug} always has a live
	// workspace row (it is 404 otherwise), so it never carries Orphan=true.
	Orphan bool `json:"orphan"`
	// SizeError is non-empty when humanize.DirSize (or stat-ing Path in the
	// first place) failed for a reason other than "does not exist" —
	// typically a permission error. Bytes is 0 and must not be trusted when
	// this is set; callers render "?" instead (docs/plans/
	// home-workspace-volume.md PR5: "エラー時はエラーにせず「?」表示").
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

	n, sizeErr := humanize.DirSize(path)
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
// A lister failure degrades gracefully to "every entry unmarked as orphan"
// (logged, not returned as an error) rather than failing the whole listing
// — GC's own record-deletion work does not depend on workspace-home
// reporting succeeding, and a transient ListWorkspaces error should not
// block it.
//
// Returns an empty (non-nil) slice, not an error, when homes/ does not
// exist yet (a fresh installation that has never dispatched a job).
func ListWorkspaceHomeSizes(runtimesDir string, lister WorkspaceSlugLister) ([]WorkspaceHomeSize, error) {
	homesDir, err := dispatcher.WorkspaceHomesDir(runtimesDir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(homesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []WorkspaceHomeSize{}, nil
		}
		return nil, err
	}

	known := map[string]bool{}
	if lister != nil {
		if summaries, lerr := lister.ListWorkspaces(); lerr != nil {
			slog.Warn("list workspace homes: ListWorkspaces failed, orphan detection degraded", "error", lerr)
		} else {
			for _, s := range summaries {
				known[s.ID] = true
			}
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
	return result, nil
}

// WorkspaceRemoveResponse is the response body for DELETE
// /api/workspaces/{slug} (docs/plans/home-workspace-volume.md Phase 4 PR5).
// The workspace row is always removed before any home-directory deletion is
// attempted (WorkspaceHandler.Remove calls Service.RemoveWorkspace first),
// so a non-empty HomeDeleteError reports a *partially* completed remove —
// deliberately allowed per the plan doc ("削除失敗... workspace 設定 (DB)
// の削除は先に完了させる (part-completed 状態を許容...)") rather than
// treated as a request failure: the response status stays 200.
type WorkspaceRemoveResponse struct {
	Status string `json:"status"`
	// HomePath/HomeBytes describe the home directory as it was found right
	// before the deletion attempt (empty/0 when RuntimesDir was not wired
	// into the handler, or slug is the reserved default workspace).
	HomePath  string `json:"home_path,omitempty"`
	HomeBytes int64  `json:"home_bytes,omitempty"`
	// HomeDeleted is true only when a home directory existed and os.RemoveAll
	// on it succeeded. false covers every other case: no RuntimesDir wired,
	// the default workspace's protected home, no home directory to begin
	// with, or a deletion failure (see HomeDeleteError for that last one).
	HomeDeleted     bool   `json:"home_deleted"`
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
func deleteWorkspaceHome(runtimesDir, slug string) (info WorkspaceHomeSize, deleted bool, err error) {
	info = computeWorkspaceHomeSize(runtimesDir, slug)

	if slug == orchestrator.DefaultWorkspaceSlug {
		return info, false, nil
	}
	if info.SizeError != "" {
		// Could not even stat/size the directory — do not attempt a blind
		// os.RemoveAll; surface the same failure instead.
		return info, false, errors.New(info.SizeError)
	}
	if !info.Exists {
		return info, false, nil // nothing to delete.
	}

	if err := os.RemoveAll(info.Path); err != nil {
		return info, false, err
	}
	return info, true, nil
}

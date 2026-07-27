package api

import (
	"context"
	"log/slog"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Workspace HOME reporting and deletion (docs/plans/home-workspace-volume.md
// Phase 4 PR5, rewired onto the engine by 論点 a-2 / PR7 of
// docs/plans/workspace-home-volume-persistence.md).
//
// PR6 made a workspace's $HOME a per-workspace docker named volume
// (dockerres.WorkspaceHomeVolumeName), which left everything in this file
// resolving a <runtimesRoot>/homes/<slug> directory the dispatcher no longer
// touches: sizes came back empty for every workspace, no orphan was ever
// detected, and `workspace remove` silently left a volume full of harness
// credentials behind. PR7 replaces the mechanism — the engine's volume API,
// via dispatcher.WorkspaceHomeVolumeStore — and keeps the policy here, where
// the workspace rows are.
//
// This package still imports no moby types (論点 a-2, D2): the store hands
// back dispatcher.WorkspaceHomeVolume, a plain struct, and WorkspaceHomeStore
// below is the narrow consumer-side interface a test can stub without an
// engine.

// WorkspaceHomeSize describes one workspace home's footprint: used both by
// GET /api/workspaces/{slug} (a single entry, WorkspaceDetail.Home) and by
// POST /api/gc's workspace_homes listing (one entry per workspace home volume
// this install has).
type WorkspaceHomeSize struct {
	Slug string `json:"slug"`

	// Volume is the engine-side name of the workspace's HOME volume,
	// dockerres.WorkspaceHomeVolumeName(installID, slug) — the same name the
	// dispatcher mounts as the harness's $HOME, and the name to pass to
	// `docker volume rm` for manual cleanup. Always set, whether or not the
	// volume currently exists.
	//
	// It REPLACES the `path` field this struct carried through PR6, rather
	// than joining it (論点 a-2, D4). A volume name is not a path, and the
	// only consumers are boid's own CLI renderers (cmd/workspace.go's
	// formatWorkspaceHomeSize / formatWorkspaceRemoveResult, cmd/gc.go's
	// printWorkspaceHomes), which this PR updates in lockstep. Keeping a
	// `path` key alive with a volume name in it would have made every one of
	// them print a lie that reads like a path; keeping BOTH would have meant
	// emitting a legacy field describing a directory the daemon stopped
	// using two PRs ago.
	//
	// On CLI/daemon version skew: this repo has no API versioning and no
	// compatibility shim — a field the peer does not know decodes to its zero
	// value, which is precisely how `boid gc`'s listing already documents its
	// own old-daemon behavior ("either the daemon was too old to report it,
	// or no workspace has ever been dispatched into yet", cmd/gc.go's
	// printWorkspaceHomes). Skew is reachable since Phase 3 made the CLI able
	// to talk to a remote daemon, so the renderers updated by this PR treat
	// an empty identifier as "not reported" and omit the parenthetical
	// instead of printing an empty one. That is the whole cost in both
	// directions: a size still renders, only unattributed.
	Volume string `json:"volume"`

	// Exists reports whether the volume is currently on the engine. false
	// means the workspace has never been dispatched into (docs/plans/
	// home-workspace-volume.md's init-on-first-dispatch contract) — not an
	// error.
	Exists bool `json:"exists"`

	// Bytes is the volume's size as the engine's GET /system/df reports it,
	// meaningful only when Exists is true and SizeError is empty. See
	// dispatcher.WorkspaceHomeVolumeStore's doc comment for what that number
	// means (du --apparent-size semantics) and why that endpoint is the only
	// one that can produce it.
	Bytes int64 `json:"bytes"`

	// Orphan is true when a workspace HOME volume exists but Slug has no
	// corresponding workspace row — a workspace removed (or never
	// assign/create'd) while its home volume survived. Only ever set by the
	// GC listing (ListWorkspaceHomeSizes); the single-slug lookup used by
	// GET /api/workspaces/{slug} always has a live workspace row (it is 404
	// otherwise), so it never carries Orphan=true.
	Orphan bool `json:"orphan"`

	// SizeError is non-empty when the size could not be determined — the
	// engine's disk-usage call failed, or it listed the volume without usage
	// data. Bytes is 0 and must not be trusted when this is set; callers
	// render "?" instead (docs/plans/home-workspace-volume.md PR5: "エラー時は
	// エラーにせず「?」表示"). A non-empty SizeError does *not* imply the
	// volume is gone or undeletable — deleteWorkspaceHome treats sizing as
	// best-effort and still attempts deletion regardless (codex PR #791
	// review, Should-fix #2).
	SizeError string `json:"size_error,omitempty"`
}

// WorkspaceSlugLister exposes the set of currently known workspace slugs,
// used to flag orphaned home volumes (a workspace HOME volume with no
// corresponding workspace row). *ProjectAppService already satisfies this via
// its existing ListWorkspaces method — no new implementation is needed, only
// threading it through as this narrower interface at the handler construction
// site (server/wire.go) so GCHandler does not need the whole ProjectService
// surface.
type WorkspaceSlugLister interface {
	ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error)
}

// WorkspaceHomeStore is the engine-backed view of workspace HOME volumes.
// *dispatcher.WorkspaceHomeVolumeStore is the only implementation;
// server/wire.go builds one from the single docker client buildRuntime
// already has and hands it to WorkspaceHandler.Homes / GCHandler.Homes.
//
// Declared here, on the consumer side, so this package can be tested with a
// map-backed stub — and typed in terms of dispatcher.WorkspaceHomeVolume
// rather than moby's volume.Volume so no moby type reaches internal/api
// (論点 a-2, D2). internal/api already imports internal/dispatcher; the
// reverse edge does not exist and must not be created, which is why the
// data type lives on the dispatcher side.
//
// A nil WorkspaceHomeStore is the feature's OFF switch, and is what the
// handlers gate on now that RuntimesDir no longer participates (論点 a-2,
// D5): server/wire.go leaves the field nil exactly when it has no docker
// client (Config.Backend injected — the test/DI path), and both handlers then
// omit home information entirely, unchanged from what an empty RuntimesDir
// used to do. The engine handle is the feature's only remaining dependency,
// so it is the honest condition; a nil interface is also the only form of
// "off" that cannot be faked by a typed-nil *client.Client, which is why
// wire.go assigns the field rather than always constructing a store.
type WorkspaceHomeStore interface {
	Get(ctx context.Context, slug string) dispatcher.WorkspaceHomeVolume
	List(ctx context.Context) ([]dispatcher.WorkspaceHomeVolume, error)
	Remove(ctx context.Context, slug string) (dispatcher.WorkspaceHomeVolume, bool, error)
}

// homeSizeFrom converts the store's plain report into the wire shape. Kept as
// one function so the two callers cannot drift on which field means what.
func homeSizeFrom(v dispatcher.WorkspaceHomeVolume) WorkspaceHomeSize {
	return WorkspaceHomeSize{
		Slug:      v.Slug,
		Volume:    v.Volume,
		Exists:    v.Exists,
		Bytes:     v.Bytes,
		SizeError: v.SizeError,
	}
}

// computeWorkspaceHomeSize reports slug's workspace HOME volume, never
// returning an error itself: every failure mode (no engine, the volume not
// existing yet, a disk-usage call that failed) is instead reflected in the
// returned WorkspaceHomeSize's Exists/SizeError fields, matching the "never
// block the rest of the response on a size lookup failure" contract PR5's
// brief sets for both `workspace show` and `boid gc`.
func computeWorkspaceHomeSize(ctx context.Context, store WorkspaceHomeStore, slug string) WorkspaceHomeSize {
	return homeSizeFrom(store.Get(ctx, slug))
}

// ListWorkspaceHomeSizes enumerates every workspace HOME volume belonging to
// this install and reports each one's size, flagging the ones with no
// corresponding workspace row as orphans (docs/plans/home-workspace-volume.md
// PR5: "workspace.yaml が消えて home だけ残った孤児はレポートのみ").
//
// Orphan detection depends on a successful WorkspaceSlugLister call. A lister
// failure means orphan status is simply unknowable for every entry — rather
// than marking every entry Orphan=true (this function's pre-#791-review
// behavior, which silently misreported every real workspace's home as an
// "orphan" on a merely transient DB hiccup), a lister failure now omits the
// listing outright: homes comes back an empty (non-nil) slice, and listErr
// carries the lister's error message so a caller can render "size listing
// unavailable: <reason>" instead of trusting bogus per-entry orphan flags.
// This preserves the invariant "every WorkspaceHomeSize actually returned has
// a trustworthy Orphan flag" (codex PR #791 review, Should-fix #3,
// selection A).
//
// err (the third return) is reserved for a genuine enumeration failure — the
// engine's volume listing failing. Unlike listErr, a non-nil err means the
// whole call failed, not just orphan detection; GC's own record-deletion work
// does not depend on workspace-home reporting succeeding, so callers should
// still treat a non-nil err as non-fatal to the rest of their own response.
//
// A sizing failure is neither of those: the store still returns the listing,
// with SizeError set per entry, because the listing is what drives orphan
// detection and losing it over a busy disk-usage call would be a much worse
// trade than an entry rendering "?".
func ListWorkspaceHomeSizes(ctx context.Context, store WorkspaceHomeStore, lister WorkspaceSlugLister) (homes []WorkspaceHomeSize, listErr string, err error) {
	volumes, err := store.List(ctx)
	if err != nil {
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

	result := make([]WorkspaceHomeSize, 0, len(volumes))
	for _, v := range volumes {
		entry := homeSizeFrom(v)
		entry.Orphan = !known[v.Slug]
		result = append(result, entry)
	}
	return result, "", nil
}

// WorkspaceRemoveResponse is the response body for DELETE
// /api/workspaces/{slug} (docs/plans/home-workspace-volume.md Phase 4 PR5).
// The workspace row is always removed before any home deletion is attempted
// (WorkspaceHandler.Remove calls Service.RemoveWorkspace first), so a
// non-empty HomeDeleteError (or HomeSizeError) reports a *partially*
// completed remove — deliberately allowed per the plan doc ("削除失敗...
// workspace 設定 (DB) の削除は先に完了させる (part-completed 状態を許容...)")
// rather than treated as a request failure: the response status stays 200.
type WorkspaceRemoveResponse struct {
	Status string `json:"status"`

	// HomeVolume/HomeBytes describe the workspace's HOME volume as it was
	// found right before the deletion attempt (empty/0 when no docker client
	// was wired into the handler, or slug is the reserved default workspace).
	// HomeBytes is only trustworthy when HomeSizeError is empty.
	//
	// HomeVolume replaces the `home_path` field this struct carried through
	// PR6 — see WorkspaceHomeSize.Volume for the reasoning and for the
	// CLI/daemon-skew consequences.
	HomeVolume string `json:"home_volume,omitempty"`
	HomeBytes  int64  `json:"home_bytes,omitempty"`

	// HomeSizeError is non-empty when the daemon could not determine
	// HomeVolume's size before attempting deletion (mirrors
	// WorkspaceHomeSize.SizeError) — independent of whether the deletion
	// itself subsequently succeeded. Split out from HomeDeleteError (codex
	// PR #791 review, Should-fix #2): the two used to be conflated into a
	// single field, so a caller could not tell "we don't know the size" (a
	// diagnostic-only hiccup) apart from "the volume is still there and
	// undeletable" (a real part-completed-remove problem worth
	// investigating).
	HomeSizeError string `json:"home_size_error,omitempty"`

	// HomeDeleted is true only when a home volume existed and the engine
	// removed it. false covers every other case: no docker client wired, the
	// default workspace's protected home, no volume to begin with, or a
	// deletion failure (see HomeDeleteError for that last one). Sizing is
	// best-effort (see HomeSizeError) and never gates whether deletion is
	// attempted — HomeDeleted can be true even when HomeSizeError is also
	// non-empty.
	HomeDeleted bool `json:"home_deleted"`

	// HomeDeleteError is non-empty only when the engine's VolumeRemove
	// failed. Never populated merely because sizing failed (see
	// HomeSizeError for that).
	//
	// The case worth naming: removing a workspace while a job is still
	// running in it. The engine answers 409 ("volume is being used by the
	// following container(s): <id>") and — measured against podman 4.9.3,
	// 2026-07-27 — keeps answering 409 with force set, so there is nothing
	// boid can do about it beyond reporting it. The row is gone and the
	// volume is not; re-running `boid workspace remove` is not an option
	// (the row is already removed, so it 404s), which makes
	// `docker volume rm <home_volume>` the follow-up. See
	// docs/{ja,en}/guide/workspace-home.md.
	HomeDeleteError string `json:"home_delete_error,omitempty"`

	// InitScriptDeleted is true only when the workspace HAD an init.sh and it
	// was removed (PR9 codex round 2, Major 1). false covers a workspace that
	// never had one, the reserved default workspace, a daemon with no init
	// script store wired, and a failed deletion — see InitScriptDeleteError
	// for that last one.
	//
	// Reported at all because leaving a workspace's init.sh behind is not a
	// harmless leftover: dispatch resolves the script from the SLUG, so the
	// next workspace created with the same name inherits it and runs it
	// against a brand-new, empty HOME volume. The row is gone by then, which
	// also means `boid workspace unset-init-script` can no longer reach it.
	InitScriptDeleted bool `json:"init_script_deleted"`

	// InitScriptDeleteError is non-empty only when the daemon tried to delete
	// the workspace's init.sh and could not. Best-effort, exactly like
	// HomeDeleteError and for the same reason: the row removal has already
	// committed, so failing the whole request would report a failure over an
	// irreversible success — and re-running `boid workspace remove` is not
	// available, since it 404s on the row that is already gone.
	//
	// The message carries the daemon-side path (WorkspaceInitScriptStore.
	// Remove wraps it in), which is what an operator needs to delete the
	// leftover by hand — and, under the container deploy, to know which
	// filesystem it is on.
	InitScriptDeleteError string `json:"init_script_delete_error,omitempty"`
}

// deleteWorkspaceInitScript removes slug's init.sh as part of
// DELETE /api/workspaces/{slug} (PR9 codex round 2, Major 1), reporting
// whether there was one to remove.
//
// Called from ProjectAppService.RemoveWorkspace, with the service mutex held
// and the row already gone (PR9 codex round 3, Major 3) — NOT from the handler
// alongside deleteWorkspaceHome below, which is where it started. See
// RemoveWorkspace for the interleaving that move closes.
//
// A nil store is not an error: the same "this daemon has no such capability"
// off switch a nil Homes gets above, answered with removed=false so the
// response never claims a deletion that was not attempted.
//
// The reserved default workspace is refused here as well as at the row level,
// the defense-in-depth deleteWorkspaceHome's doc comment argues for: the guard
// belongs with the destructive act, not only with the caller that currently
// happens to precede it.
func deleteWorkspaceInitScript(store WorkspaceInitScriptStore, slug string) (bool, error) {
	if store == nil || slug == orchestrator.DefaultWorkspaceSlug {
		return false, nil
	}
	return store.Remove(slug)
}

// deleteWorkspaceHome removes slug's workspace HOME volume (docs/plans/
// home-workspace-volume.md Phase 4 PR5, DELETE /api/workspaces/{slug}).
//
// The reserved default workspace is refused unconditionally here as defense
// in depth: ProjectAppService.RemoveWorkspace already rejects removing the
// default workspace's *row* before WorkspaceHandler.Remove ever calls this
// function, so in practice this branch should be unreachable — but home
// deletion gets its own independent guard anyway, so a future caller of this
// function (or a bug in the row-level guard) cannot accidentally destroy the
// default workspace's persistent $HOME. The guard is placed BEFORE the store
// call, not after: the store's Remove would otherwise have already issued the
// engine's VolumeRemove by the time this function got to decide.
//
// Sizing is best-effort (codex PR #791 review, Should-fix #1/#2) and never
// blocks the deletion attempt; that contract now lives inside
// dispatcher.WorkspaceHomeVolumeStore.Remove, which issues VolumeRemove
// regardless of whether it could size — or even confirm the existence of —
// the volume first.
func deleteWorkspaceHome(ctx context.Context, store WorkspaceHomeStore, slug string) (info WorkspaceHomeSize, deleted bool, err error) {
	if slug == orchestrator.DefaultWorkspaceSlug {
		return homeSizeFrom(store.Get(ctx, slug)), false, nil
	}
	v, deleted, err := store.Remove(ctx, slug)
	return homeSizeFrom(v), deleted, err
}

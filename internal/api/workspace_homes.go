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

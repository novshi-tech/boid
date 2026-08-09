package apiwire

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

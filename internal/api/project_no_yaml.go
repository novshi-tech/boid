package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// noYAMLProjectIDPrefix marks an id derived from a git URL rather than read
// from a committed project.yaml's `id:` field (docs/plans/
// workspace-default-project.md 決定3/論点g). Distinguishes it at a glance
// from a UUID (36 chars) or a short hand-authored project.yaml id, without
// colliding with either — no existing id scheme in this codebase starts with
// "url-".
//
// This is an alias for orchestrator.URLDerivedProjectIDPrefix, not an
// independent value — LoadAll/FetchProject's reload fallback gates on that
// exact prefix (Codex review, PR5 round 2 Major) to tell a no-project.yaml
// registration apart from an ordinary project whose project.yaml was
// deleted upstream by mistake, so this package must never drift from it.
const noYAMLProjectIDPrefix = orchestrator.URLDerivedProjectIDPrefix

// deriveProjectIDFromURL derives a stable project id for a project.yaml-less
// registration (docs/plans/workspace-default-project.md 決定3) from
// normalizedURL — which MUST already be the https:// or file:// form
// dispatcher.NormalizeOriginURL produces (CreateProjectFromGitURL always
// normalizes before this is ever called).
//
// The hash input is dispatcher.RepoSlugFromOriginURL's output (the same
// host/owner/repo normalization gitgateway.NewRepoKey applies), NOT
// normalizedURL itself: NormalizeOriginURL passes an https:// URL through
// unchanged, `.git` suffix and all, so hashing the raw normalized URL would
// let "https://host/o/r" and "https://host/o/r.git" register as two
// different projects — weaker dedup than today's project.yaml `id:` PK
// collision (决定3, fable review M1). RepoSlugFromOriginURL strips the
// suffix and produces one canonical slug for both forms.
//
// file:// URLs cannot be slugified (repoSlugFromOriginURL has no file://
// case — it falls into the scp-like default branch and fails to find an
// "@") — this is a pre-existing limitation of the same normalization
// gitgateway grants already accept (see docs/plans/
// workspace-default-project.md 論点c 2巡目レビュー確認事項), not a new gap
// this PR introduces. Callers must surface this error as a clear rejection,
// not attempt any further fallback.
//
// The returned id is "url-" + the first 16 hex characters (64 bits) of the
// slug's sha256 — long enough hex-64 is unwieldy for CLI use; 64 bits of a
// stable hash is more than sufficient collision resistance for a single
// user's project count (論点g). The prefix both signals "this project has no
// project.yaml id" and cannot collide with a UUID (36 chars, contains '-' at
// fixed positions but never starts with "url-") or a short hand-authored
// project.yaml id (existing convention has never used this prefix).
func deriveProjectIDFromURL(normalizedURL string) (string, error) {
	slug, err := dispatcher.RepoSlugFromOriginURL(normalizedURL)
	if err != nil {
		return "", fmt.Errorf("cannot derive a project id from %q: %w (project.yaml-less registration is not supported for this URL form; commit a .boid/project.yaml with an explicit id instead)", dispatcher.SanitizeURLForLogging(normalizedURL), err)
	}
	sum := sha256.Sum256([]byte(slug))
	return noYAMLProjectIDPrefix + hex.EncodeToString(sum[:])[:16], nil
}

// synthesizeNoYAMLProjectMeta builds the ProjectMeta for a project.yaml-less
// registration (docs/plans/workspace-default-project.md PR5, 論点b/c/d).
// Called by CreateProjectFromGitURL exactly when gitShowHEAD's failure
// classifies as GitHeadReadFailurePathAbsent (the ".boid/project.yaml was
// never committed" case — every other failure kind keeps failing
// registration outright, unchanged).
//
// gitURL is the already-normalized (https:// or file://) URL;
// workspaceSlug's existence has already been verified by the caller; name is
// the caller's already-resolved name (nameOverride if given, else
// orchestrator.DeriveProjectNameFromURL(gitURL)) — this function does not
// re-derive it, only validates/uses it, since CreateProjectFromGitURL needs
// that same name value regardless of which project.yaml/no-yaml branch it
// takes (it names the bare-repo directory too).
//
// Returns a *StatusError on any rejection, ready to propagate straight back
// to the caller.
func (s *ProjectAppService) synthesizeNoYAMLProjectMeta(gitURL, name, workspaceSlug string, nameWasExplicit bool) (*orchestrator.ProjectMeta, error) {
	id, err := deriveProjectIDFromURL(gitURL)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// 論点d: the workspace must have a USABLE default project definition —
	// not merely "a default definition object exists". s.Workspaces unwired,
	// or the workspace having no row at all, is treated identically to "no
	// usable default": there is nothing to fall back to.
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "この workspace には default project 定義が無いので、project.yaml の無い repository は登録できません (workspace store not wired)"}
	}
	wsMeta, err := s.Workspaces.Load(workspaceSlug)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("この workspace (%q) には default project 定義が無いので、project.yaml の無い repository は登録できません; `boid workspace update %s` で task_behaviors / default_task_behavior を設定してください", workspaceSlug, workspaceSlug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("load workspace %q: %v", workspaceSlug, err)}
	}

	// 論点b m2: name-collision check across ALL registered projects — the
	// same set apply's ambiguity check (resolveProjectByNameExact over
	// snapshotRegisteredProjects) reads. A repo-basename-derived name
	// collides easily across workspaces; an auto-derived name that collides
	// is refused outright (ask for --name), an explicitly-given --name that
	// still collides is allowed through with a warning (the caller made an
	// informed choice).
	snapshot, err := s.snapshotRegisteredProjects()
	if err != nil {
		return nil, err
	}
	if matches := resolveProjectByNameExact(snapshot, name); len(matches) > 0 {
		matchIDs := make([]string, len(matches))
		for i, m := range matches {
			matchIDs[i] = m.ID
		}
		if !nameWasExplicit {
			return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
				"project name %q derived from the URL already matches %d registered project(s) (%v); pass --name to choose a distinct name (workspace apply/export identify projects by name, and a collision detaches the wrong one)",
				name, len(matches), matchIDs,
			)}
		}
		slog.Warn("project.yaml-less registration: --name collides with an already-registered project's name; workspace export/apply name resolution may become ambiguous",
			"name", name, "colliding_project_ids", matchIDs)
	}

	// 論点d (2巡目レビュー m2): the check is NOT "does ws have task_behaviors"
	// — it is "would ResolveBehavior's own default-resolution branch actually
	// succeed" for a behavior-unspecified task creation against the merged
	// result GetWithWorkspace will dynamically produce for this project once
	// registered. Mirrors behavior_resolve.go:104-117 exactly via
	// orchestrator.DefaultBehaviorResolvable.
	candidate := &orchestrator.ProjectMeta{
		ID:                  id,
		Name:                name,
		TaskBehaviors:       wsMeta.TaskBehaviors,
		DefaultTaskBehavior: wsMeta.DefaultTaskBehavior,
		BaseBranch:          wsMeta.BaseBranch,
		ForkPoint:           wsMeta.ForkPoint,
	}
	if !orchestrator.DefaultBehaviorResolvable(candidate) {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf(
			"この workspace (%q) には default project 定義が無いので、project.yaml の無い repository は登録できません; default_task_behavior を設定するか、'supervisor' という名前の behavior を task_behaviors に追加してください",
			workspaceSlug,
		)}
	}

	// The cached meta itself stays minimal (ID + Name only) — TaskBehaviors /
	// BaseBranch / ForkPoint / DefaultTaskBehavior are deliberately left
	// empty so GetWithWorkspace's existing dynamic merge (決定2) fills them
	// in from whatever the workspace's CURRENT default project definition is
	// at hydrate time, not a snapshot taken here at registration time.
	//
	// NameSource records how `name` was obtained, for `project show
	// --explain` (docs/plans/workspace-default-project.md 論点e, PR6):
	// "explicit" when the caller passed --name, "url" when it was derived
	// from the git URL (DeriveProjectNameFromURL, the caller's default).
	nameSource := "url"
	if nameWasExplicit {
		nameSource = "explicit"
	}
	return &orchestrator.ProjectMeta{ID: id, Name: name, NameSource: nameSource}, nil
}

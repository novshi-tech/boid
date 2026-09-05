package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file is the policy half of the workspace init.sh surface —
// everything the dispatcher-side store deliberately does not decide. See
// internal/api/workspace_init_script.go for the endpoint contract and
// dispatcher.WorkspaceInitScriptStore for the mechanism.

// GetWorkspaceInitScript implements ProjectService.GetWorkspaceInitScript.
//
// The workspace row is checked FIRST, so a typo'd slug is a 404 rather than
// "this workspace has no init.sh" — the two are indistinguishable to a reader
// of the second message and only one of them is worth acting on.
func (s *ProjectAppService) GetWorkspaceInitScript(slug string) (*WorkspaceInitScript, error) {
	if err := s.checkWorkspaceInitScriptPreconditions(slug); err != nil {
		return nil, err
	}
	return s.loadWorkspaceInitScript(slug)
}

// SetWorkspaceInitScript implements ProjectService.SetWorkspaceInitScript.
//
// The If-Match check and the write run under s.mu, the same mutex every
// other workspace-mutating entry point takes — closing the read-then-write
// window against another caller of this daemon (not against a hand-edit of
// the file inside the container, which is exactly the workflow this
// endpoint exists to replace).
//
// It does not exclude a concurrent dispatch, deliberately:
// resolveWorkspaceHome re-reads and re-hashes the script under its own
// flock before running it, so a write landing mid-init is either picked
// up in full or missed entirely — never half-applied.
func (s *ProjectAppService) SetWorkspaceInitScript(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error) {
	if err := s.checkWorkspaceInitScriptPreconditions(slug); err != nil {
		return nil, err
	}
	// Validate BEFORE taking the lock and before touching the filesystem: a
	// refusal here must leave the workspace exactly as it was.
	if err := validateWorkspaceInitScriptContent(content); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("workspace %q: %v", slug, err)}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Asked again, under the lock: the check above ran before the mutex was
	// taken, so a concurrent `boid workspace remove` could otherwise leave
	// this write to re-create an init.sh (and its directory) for a slug
	// with no row, which nothing could then reach or clean up. s.mu is the
	// same exclusion RemoveWorkspace takes, so the removal is either
	// entirely before this line (and the re-check sees it) or entirely
	// after this function returns.
	if err := s.confirmWorkspaceRowExists(slug); err != nil {
		return nil, err
	}

	current, err := s.loadWorkspaceInitScript(slug)
	if err != nil {
		return nil, err
	}
	if !force {
		if ifMatch == "" {
			return nil, &StatusError{
				Code: http.StatusPreconditionRequired,
				Message: fmt.Sprintf("If-Match is required (current revision %q); pass ?force=true to write unconditionally",
					current.Revision),
			}
		}
		if ifMatch != current.Revision {
			return nil, &StatusError{
				Code: http.StatusPreconditionFailed,
				Message: fmt.Sprintf("revision mismatch: If-Match %q does not match the current revision %q",
					ifMatch, current.Revision),
			}
		}
	}

	return s.writeWorkspaceInitScriptLocked(slug, content, current)
}

// writeWorkspaceInitScriptLocked performs the write (or the clear) and
// describes what it did. s.mu must be held.
//
// Split out from SetWorkspaceInitScript so ApplyWorkspace's hydration can
// reuse the same three-way outcome without also inheriting the If-Match
// contract, which an envelope document has no way to express.
func (s *ProjectAppService) writeWorkspaceInitScriptLocked(slug string, content []byte, current *WorkspaceInitScript) (*WorkspaceInitScriptResult, error) {
	result := &WorkspaceInitScriptResult{Slug: slug, Path: current.Path}

	if workspaceInitScriptContentIsAbsent(content) {
		removed, err := s.InitScripts.Remove(slug)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		result.Action = WorkspaceInitScriptCleared
		if !removed {
			result.Action = WorkspaceInitScriptUnchanged
		}
		result.Revision = WorkspaceInitScriptAbsentRevision
		return result, nil
	}

	// The store is asked to publish the script even when the bytes it
	// already holds are identical: Write also chmods the per-workspace
	// directory to 0700 on every call, repairing a directory a hand-managed
	// workflow may have left group-writable. Short-circuiting on identical
	// content would skip that repair. The reported action is still
	// "unchanged" — it describes the CONTENT, so no init re-run follows —
	// even though the mode may have just been fixed.
	action := WorkspaceInitScriptWritten
	if current.Exists && bytes.Equal(current.Content, content) {
		action = WorkspaceInitScriptUnchanged
	}
	if err := s.InitScripts.Write(slug, content); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	result.Action = action
	result.Revision = workspaceInitScriptRevision(content, true)
	result.Bytes = len(content)
	return result, nil
}

// checkWorkspaceInitScriptPreconditions rejects an invalid slug, an unwired
// store, and a workspace that has no row.
//
// Cheap refusals only. The row check it ends with is a courtesy — it makes a
// typo'd slug a 404 without taking the service mutex — and NOT the one a write
// may rely on: see confirmWorkspaceRowExists and SetWorkspaceInitScript's
// second call to it.
func (s *ProjectAppService) checkWorkspaceInitScriptPreconditions(slug string) error {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.InitScripts == nil {
		return &StatusError{
			Code: http.StatusNotImplemented,
			Message: "this daemon cannot read or write a workspace init.sh: no init script store is wired into the workspace API " +
				"(a test/DI daemon)",
		}
	}
	if s.Workspaces == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}
	return s.confirmWorkspaceRowExists(slug)
}

// confirmWorkspaceRowExists reports whether slug still has a workspaces
// row, as a *StatusError the handler can answer with directly.
//
// Uses Workspaces.LoadWithRevision rather than GetWorkspace: this surface
// only needs to know the workspace exists, not GetWorkspace's full detail
// view. Split out from the precondition block above because it also runs
// a second time under s.mu in SetWorkspaceInitScript, where its answer is
// only good for as long as a concurrent RemoveWorkspace is excluded.
func (s *ProjectAppService) confirmWorkspaceRowExists(slug string) error {
	if _, _, err := s.Workspaces.LoadWithRevision(slug); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
		}
		return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return nil
}

// loadWorkspaceInitScript reads slug's script and derives its revision.
// Callers must have run checkWorkspaceInitScriptPreconditions first.
func (s *ProjectAppService) loadWorkspaceInitScript(slug string) (*WorkspaceInitScript, error) {
	path, err := s.InitScripts.Path(slug)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	data, exists, err := s.InitScripts.Read(slug)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &WorkspaceInitScript{
		Slug:     slug,
		Exists:   exists,
		Content:  data,
		Revision: workspaceInitScriptRevision(data, exists),
		Path:     path,
	}, nil
}

// exportWorkspaceInitScript returns slug's init.sh for an envelope export,
// or "" when the workspace has none (an explicit `init_script: ""`, see
// WorkspaceEnvelopeSpec.InitScript).
//
// A read failure is an error rather than an empty string: silently
// exporting "" for a script that is actually there would produce a backup
// which, applied, DELETES the workspace's init.sh — indistinguishable from
// a legitimate script-less export once written to a file.
//
// An unwired store is the exception: a daemon with no store has no
// init.sh to export for any workspace, so this isn't a read failure.
func (s *ProjectAppService) exportWorkspaceInitScript(slug string) (string, error) {
	if s.InitScripts == nil {
		return "", nil
	}
	data, exists, err := s.InitScripts.Read(slug)
	if err != nil {
		return "", &StatusError{
			Code: http.StatusInternalServerError,
			Message: fmt.Sprintf("workspace %q: read init.sh for export: %v "+
				"(refused rather than exporting an empty init_script, which on apply would DELETE the workspace's script)", slug, err),
		}
	}
	if !exists {
		return "", nil
	}
	return string(data), nil
}

// checkWorkspaceEnvelopeIsApplicable refuses to emit an exported document
// that this same daemon's `workspace apply` would reject for its size.
//
// Without this, a workspace with a large env plus a cap-sized init.sh
// could export successfully yet exceed workspaceBodyMaxBytes as a single
// document, so the backup would fail to restore — discovered at the worst
// possible moment. The refusal is on the export side because raising
// apply's cap would raise the daemon's memory ceiling for every workspace
// document, and capping metadata fields individually would add refusals
// on four other write paths for a class of workspace nobody has.
//
// This does not bound the metadata half on its own (a workspace whose env
// alone approaches the body cap could never round-trip regardless); it
// only guarantees the SCRIPT is never the reason an export stops fitting.
// See TestWorkspaceInitScriptMaxBytes_IsDerivedFromTheApplyBodyCap and
// TestWorkspaceInitScript_TheLargestAcceptedScriptSurvivesExportAndApply.
//
// The size measured is one marshaled document, since one document per
// request is what `boid workspace apply` POSTs.
func checkWorkspaceEnvelopeIsApplicable(slug string, document []byte, initScriptBytes int) error {
	if len(document) <= workspaceBodyMaxBytes {
		return nil
	}
	return &StatusError{
		Code: http.StatusConflict,
		Message: fmt.Sprintf(
			"workspace %q exports to a %d-byte document, over the %d-byte limit `boid workspace apply` reads a document under "+
				"(its init.sh accounts for %d of the source bytes, at most %d of which are allowed) — export refused rather than "+
				"written, because the file would not restore: shrink the workspace's env/host_commands/extra_repos, or its init.sh",
			slug, len(document), workspaceBodyMaxBytes, initScriptBytes, workspaceInitScriptMaxBytes),
	}
}

// validateWorkspaceApplyInitScript is ApplyWorkspace's pre-transaction check
// on spec.init_script.
//
// Kept separate from the hydration below because a refusal issued after
// the commit would have to be phrased as a partial apply, and would lose
// its status code (ApplyWorkspace wraps that post-commit error, and
// writeServiceError's type assertion does not see through a wrap). Runs
// unconditionally on dryRun for the same reason ApplyWorkspace's doc
// comment gives for validateHostCommandRefs: a dry run that skips a check
// reports success on a document the real apply goes on to reject.
func validateWorkspaceApplyInitScript(apply *orchestrator.WorkspaceEnvelopeApply) error {
	if !apply.FieldsPresent["init_script"] {
		return nil
	}
	if err := validateWorkspaceInitScriptContent([]byte(apply.Envelope.Spec.InitScript)); err != nil {
		return &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("workspace %q: spec.init_script: %v", apply.Envelope.Metadata.Name, err),
		}
	}
	return nil
}

// applyWorkspaceInitScript hydrates spec.init_script after a committed
// apply, returning the action to report (empty when the document did not
// carry the field) and any warning for the caller to surface. The content
// has already been validated by validateWorkspaceApplyInitScript, before
// the transaction opened.
//
// This runs AFTER the transaction commits: a file write and a DB
// transaction cannot commit together, and this ordering fixes the shape of
// a partial apply to "the metadata landed, the script did not"
// (recoverable by re-running the same apply), rather than leaving a
// script whose workspace was never created.
//
// dryRun computes the action and writes nothing, the same discipline
// ApplyWorkspaceEnvelope's rollback gives the DB half.
func (s *ProjectAppService) applyWorkspaceInitScript(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (action string, warning string, err error) {
	if !apply.FieldsPresent["init_script"] {
		return "", "", nil
	}
	slug := apply.Envelope.Metadata.Name
	if s.InitScripts == nil {
		return "", fmt.Sprintf(
			"workspace %q: spec.init_script was ignored — this daemon has no init script store wired", slug), nil
	}

	content := []byte(apply.Envelope.Spec.InitScript)
	current, err := s.loadWorkspaceInitScript(slug)
	if err != nil {
		return "", "", err
	}
	if dryRun {
		switch {
		case workspaceInitScriptContentIsAbsent(content):
			if !current.Exists {
				return WorkspaceInitScriptUnchanged, "", nil
			}
			return WorkspaceInitScriptCleared, "", nil
		case current.Exists && bytes.Equal(current.Content, content):
			return WorkspaceInitScriptUnchanged, "", nil
		default:
			return WorkspaceInitScriptWritten, "", nil
		}
	}

	result, err := s.writeWorkspaceInitScriptLocked(slug, content, current)
	if err != nil {
		return "", "", err
	}
	return result.Action, "", nil
}

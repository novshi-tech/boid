package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d): the
// policy half of the workspace init.sh surface — everything the
// dispatcher-side store deliberately does not decide.
//
// See internal/api/workspace_init_script.go for the endpoint contract and
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
// The If-Match check and the write are performed under s.mu — the same mutex
// every other workspace-mutating entry point takes (see the field's doc
// comment). That closes the read-then-write window against another caller of
// THIS daemon; it does not, and cannot, close it against somebody editing the
// file inside the daemon's container by hand, which is exactly the workflow
// this endpoint exists to replace.
//
// It does not exclude a concurrent DISPATCH, deliberately. resolveWorkspaceHome
// re-reads and re-hashes the script under its own flock before running it (the
// PR #787 TOCTOU fix), so a write landing mid-init is either picked up in full
// by that re-read or missed entirely — never half-applied — and the completion
// marker records whichever bytes actually ran. Taking the init lock here would
// buy an ordering nobody needs at the cost of making a CLI command block on a
// multi-minute toolchain build.
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

	// Asked AGAIN, under the lock (PR9 codex round 2, Major 2).
	//
	// The check above ran before the mutex was taken, so a `boid workspace
	// remove` landing in between left this write to proceed against a
	// workspace that no longer existed — an init.sh, and a config directory to
	// hold it, for a slug with no row. Nothing could then reach it: every
	// route on /api/workspaces/{slug}/init-script 404s on the missing row, so
	// `unset-init-script` had nothing to target, and the next workspace
	// created with that name would have inherited the file. ?force=true made
	// it certain rather than merely likely — the revision comparison that
	// might otherwise have failed is skipped.
	//
	// This is PR8's fix applied to the same shape of problem: Runner.
	// ImportWorkspaceHome asks ConfirmWorkspaceExists immediately after
	// registering the migration, precisely so the answer cannot go stale
	// before it is acted on. Here the exclusion is s.mu, which RemoveWorkspace
	// also takes, so a removal is either entirely before this line (and the
	// re-check sees it) or entirely after this function returns.
	//
	// Major 1's cleanup on `workspace remove` does not cover this on its own:
	// that cleanup runs first, and this write would re-create the file behind
	// it.
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

	// The store is asked to publish the script even when the bytes it already
	// holds are identical (PR9 codex round 3, Minor).
	//
	// Write is "make this file be exactly this", and the file's MODE is part
	// of that: it chmods the per-workspace directory to 0700 on every write,
	// because os.MkdirAll only applies its mode to directories it creates and
	// the pre-PR9 hand-managed workflow left 0775 group-writable directories
	// behind on the install this plan doc migrates. Short-circuiting on
	// identical content skipped exactly that repair, so re-setting (or
	// re-applying) the script an operator already has — the obvious thing to
	// do, and the whole point of an idempotent apply — left the directory
	// anyone in the group could swap init.sh inside of.
	//
	// The action is still "unchanged", because that word describes the
	// CONTENT: the revision and the completion marker's hash are the same
	// either way, so no init re-run follows. What is lost is the file's mtime
	// as a record of when the content last changed, which is worth less than a
	// mode this function can vouch for.
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

// confirmWorkspaceRowExists reports whether slug still has a workspaces row,
// as a *StatusError the handler can answer with directly.
//
// It uses Workspaces.LoadWithRevision rather than GetWorkspace: all this
// surface needs to know is that the workspace exists, and GetWorkspace
// additionally builds a detail view out of the project repository, which is a
// dependency the init-script endpoints otherwise do not have.
//
// Split out from the precondition block above because WHERE it is asked is the
// whole of Major 2's fix: the same question has to be asked a second time
// under s.mu, since its answer is only good for as long as a concurrent
// RemoveWorkspace is excluded.
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

// exportWorkspaceInitScript returns slug's init.sh for an envelope export, or
// "" when the workspace has none (which the envelope emits as
// `init_script: ""` — an explicit "this workspace has no script", see
// WorkspaceEnvelopeSpec.InitScript).
//
// A read failure is an ERROR rather than an empty string: silently exporting
// "" for a script that is actually there produces a backup which, applied,
// DELETES the workspace's init.sh. That is the one outcome a backup must never
// have, and it is indistinguishable from a legitimate export of a script-less
// workspace once written to a file.
//
// An unwired store is the exception, and only because it is not a failure to
// read anything: a daemon with no store has no init.sh to export for any
// workspace, and failing every export on a test/DI daemon would take out a
// surface that has nothing to do with this one.
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

// checkWorkspaceEnvelopeIsApplicable refuses to emit an exported document that
// this same daemon's `workspace apply` would reject for its size (PR9 codex
// round 3, Major 2).
//
// # What it establishes
//
// "Every document `boid workspace export` emits can be applied." Before this
// check that was arithmetic rather than a property:
// workspaceInitScriptEnvelopeAllowance reserved 64 KiB for the non-script half
// of the document and NOTHING enforced it, while the script's own share was
// capped. A workspace with, say, a 400 KiB env value plus a cap-sized script
// exported at ~1,040 KiB, over the 1 MiB workspaceBodyMaxBytes that apply reads
// the body under — an export that reported success and a restore that answered
// 400 while reading the request body, discovered at the worst possible moment.
//
// # Why the refusal is on the EXPORT side
//
// Three ways to close the gap were available and only this one closes it.
//
// Raising apply's cap cannot: the metadata half is bounded only by that same
// cap on the way in, so whatever apply accepts, its own export is larger, and
// the next cap is beaten by the next document. Capping the metadata fields
// individually would bound it, at the cost of new refusals on
// create/update/import/apply — four surfaces, for a class of workspace nobody
// has, to protect a fifth.
//
// This check adds no refusal to any WRITE path. It changes an export that
// silently produced an unrestorable file into one that says so, naming the
// workspace and both numbers, while the file the operator would have relied on
// never existed in the first place. The pre-existing precedent is right above
// it: export already refuses (409) a workspace whose project names would not
// resolve back on apply, for exactly this reason.
//
// # What it does NOT bound
//
// The metadata half can still be too large — that is the case this reports. It
// is a pre-existing property of the envelope (a workspace whose env alone
// approaches the body cap could never round-trip, init.sh or no init.sh), and
// the script cap is what keeps the SCRIPT from ever being the reason. Both
// halves of that split are now asserted:
// TestWorkspaceInitScriptMaxBytes_IsDerivedFromTheApplyBodyCap on the
// arithmetic, TestWorkspaceInitScript_TheLargestAcceptedScriptSurvivesExportAndApply
// on a real workspace whose meta fills the allowance.
//
// The size measured is one MARSHALED document, because one document per
// request is what `boid workspace apply` POSTs (it splits a multi-document
// file first) — not the size of the whole export file, which has no cap
// anywhere.
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

// validateWorkspaceApplyInitScript is ApplyWorkspace's PRE-transaction check
// on spec.init_script.
//
// It is separate from the hydration below, and that separation is the point.
// The hydration runs after the commit, so a refusal issued from there would
// have to be phrased as "the metadata was applied, the script was not" — an
// honest report of a partial apply, and the wrong answer entirely for a
// document that was unusable before anything was touched. It would also lose
// its status code: ApplyWorkspace wraps that post-commit error, and
// writeServiceError's type assertion does not see through a wrap, so a 400
// would reach the operator as a 500.
//
// Runs unconditionally on dryRun, for the reason ApplyWorkspace's doc comment
// already gives for validateHostCommandRefs: a dry run that skips a check
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

// applyWorkspaceInitScript hydrates spec.init_script after a committed apply,
// returning the action to report (empty when the document did not carry the
// field) and any warning for the caller to surface. The content has already
// been validated by validateWorkspaceApplyInitScript, before the transaction
// opened.
//
// # Ordering, and why this is not atomic with the DB
//
// A file write and a database transaction cannot commit together, so one of
// them lands first and the other can fail after it. This runs AFTER the
// transaction commits, which fixes the shape of a partial apply to "the
// metadata landed, the script did not" — recoverable by re-running the same
// apply, and reported as an error naming exactly that. The other order would
// leave a script whose workspace was never created.
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

package apiwire

// WorkspaceInitScriptContentType is the media type the init script is served
// with, and the canonical one to send when writing it.
//
// text/x-shellscript rather than text/plain because the content IS a shell
// script and saying so costs nothing. It is not REQUIRED on the way in: the
// route refuses only the structured-data formats whose arrival means the
// caller is sending something else entirely, and accepts everything else
// including an absent declaration — see workspaceInitScriptMediaTypeError.
const WorkspaceInitScriptContentType = "text/x-shellscript"

// WorkspaceInitScriptAbsentRevision is the revision of a workspace that has
// NO init.sh.
//
// A sentinel rather than the empty string, because "" already means "the
// caller sent no If-Match at all" (428). With a real value for the absent
// state, creating a workspace's first script is guarded the same way replacing
// one is: `boid workspace set-init-script` reads first, gets this back, sends
// it as If-Match, and a script that appeared in between makes the write a 412
// instead of a silent overwrite.
//
// It cannot collide with a real revision: those are 32 hex characters
// (workspaceInitScriptRevision).
const WorkspaceInitScriptAbsentRevision = "absent"

// The three values WorkspaceInitScriptResult.Action (and
// orchestrator.WorkspaceApplyResult.InitScriptAction) can take.
const (
	// WorkspaceInitScriptWritten: the script was persisted (created or
	// replaced).
	WorkspaceInitScriptWritten = "written"
	// WorkspaceInitScriptCleared: the workspace's init.sh was removed, so it
	// is back in the pass-through class.
	WorkspaceInitScriptCleared = "cleared"
	// WorkspaceInitScriptUnchanged: the request asked for the state the
	// workspace was already in. Reported rather than folded into "written"
	// because a re-applied backup should not read as a change — and because
	// the completion marker's hash is unchanged either way, so no init re-run
	// follows.
	WorkspaceInitScriptUnchanged = "unchanged"
)

// WorkspaceInitScriptResult is PUT /api/workspaces/{slug}/init-script's
// response body.
type WorkspaceInitScriptResult struct {
	Slug string `json:"slug"`
	// Action is one of WorkspaceInitScriptWritten / Cleared / Unchanged.
	Action string `json:"action"`
	// Revision is the state AFTER the call — a content hash, or the absent
	// sentinel when the script was cleared. A caller editing in a loop can
	// carry it straight into the next If-Match.
	Revision string `json:"revision"`
	// Bytes is the persisted size, 0 when cleared.
	Bytes int `json:"bytes"`
	// Path is the daemon-side location, reported so an operator is never left
	// guessing which filesystem the answer refers to.
	Path string `json:"path,omitempty"`
}

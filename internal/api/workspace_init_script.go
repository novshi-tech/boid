package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// This file is the API half: GET/PUT /api/workspaces/{slug}/init-script.
//
// A workspace's init.sh installs the harness CLI and toolchain into the
// workspace HOME volume. It is read by the daemon from the daemon's own
// $XDG_CONFIG_HOME, which once the daemon runs in a container is inside a
// volume no editor on the host can reach — hence an HTTP surface for it,
// following the same conventions `boid config get/set/apply/edit` uses
// for config.yaml: raw bytes with a media type rather than a JSON
// envelope, an ETag on the read, If-Match on the write, ?force=true to
// opt out.
//
// The routes hang off {slug} so the target is a path parameter like every
// other per-workspace operation; see
// TestWorkspaceHandler_InitScriptRoutesDoNotShadowTheSlugRoutes for why a
// workspace named "init-script" still works.

// The size budget for a workspace init.sh.
//
// There is a cap at all because the daemon buffers the whole body — it
// hashes it into the completion marker and wraps it in a heredoc inside
// the init container's wrapper script — so an unbounded body is an
// unbounded allocation.
//
// It is 128 KiB rather than the 1 MiB it started as (matching
// configBodyMaxBytes/workspaceBodyMaxBytes) because that larger cap
// accepted scripts the daemon could not give back: `boid workspace
// export` carries the script inside a yaml envelope, and `boid workspace
// apply` reads that envelope through the same workspaceBodyMaxBytes. A
// document containing a 1 MiB script is necessarily larger than 1 MiB, so
// a maximum-size script the API accepted produced an export its own apply
// rejected — discovered at restore time. Lowering this cap (rather than
// raising apply's, which would raise the daemon's memory ceiling for
// every workspace document) closes the gap at no cost: real scripts are a
// few KB, and 128 KiB is still fifteen times the largest one that exists.
//
// The budget is asserted by
// TestWorkspaceInitScriptMaxBytes_IsDerivedFromTheApplyBodyCap and the
// whole path by
// TestWorkspaceInitScript_TheLargestAcceptedScriptSurvivesExportAndApply.
const (
	// workspaceInitScriptMaxBytes caps an init.sh request body.
	workspaceInitScriptMaxBytes = 128 << 10 // 128 KiB

	// workspaceInitScriptWorstCaseYAMLExpansion is how much larger a script
	// can get when yaml.Marshal writes it into an envelope's spec.init_script.
	// Measured (not guessed): the worst case is a one-character-line script
	// under literal-block style, which re-emits each line at an 8-space
	// indent (5.0018x). See
	// TestWorkspaceInitScript_WorstCaseYAMLExpansionIsNotUnderstated. 6
	// rather than 5 leaves ~20% margin for a yaml.v3 style change.
	workspaceInitScriptWorstCaseYAMLExpansion = 6

	// workspaceInitScriptEnvelopeAllowance is the room reserved in the same
	// document for everything that is NOT the script: apiVersion, kind,
	// metadata, and the workspace's own host_commands / env / allowed_domains
	// / extra_repos / container_image / capabilities / projects.
	//
	// 64 KiB against a measured 233 bytes for an empty workspace — the
	// margin is for meta fields, which have no individual caps of their own
	// (only workspaceBodyMaxBytes bounds them). It is a budget, not a limit:
	// nothing refuses a workspace whose metadata exceeds it — the guarantee
	// that an export is applicable is enforced on the finished document
	// instead, by checkWorkspaceEnvelopeIsApplicable.
	workspaceInitScriptEnvelopeAllowance = 64 << 10
)

// WorkspaceInitScriptStore is the daemon-side capability the init-script
// surface needs: read, write and remove a workspace's init.sh at the path
// the dispatcher reads it from.
//
// Declared here, on the consumer side, and implemented by
// dispatcher.WorkspaceInitScriptStore — the same split
// WorkspaceHomeStore/WorkspaceHomeImporter use: the mechanism (where the
// file is, how it is published) lives with the code that reads it, the
// policy (If-Match, the cap, the NUL refusal, empty-means-absent) lives
// here with the workspace rows.
//
// A nil value turns the feature off — the route answers 501 and `apply`
// warns rather than silently dropping a spec.init_script — for the
// DI/test daemons that have no dispatcher wiring.
type WorkspaceInitScriptStore interface {
	// Path returns the absolute path of slug's init.sh ON THE DAEMON. It is
	// reported to the operator, whose editor may well be on a different
	// machine; see the CLI's output for how it is labelled.
	Path(slug string) (string, error)
	// Read returns slug's init.sh, with exists=false and no error when the
	// workspace has none.
	Read(slug string) (data []byte, exists bool, err error)
	// Write publishes data as slug's init.sh, atomically.
	Write(slug string, data []byte) error
	// Remove deletes slug's init.sh, reporting whether there was one.
	Remove(slug string) (removed bool, err error)
}

// WorkspaceInitScript is one workspace's init.sh as the API sees it.
type WorkspaceInitScript struct {
	Slug string
	// Exists is false for a workspace in the pass-through class (no init.sh).
	// Content is then empty and Revision is WorkspaceInitScriptAbsentRevision.
	Exists  bool
	Content []byte
	// Revision is the ETag: a content hash, or the absent sentinel.
	Revision string
	// Path is where the DAEMON keeps it.
	Path string
}

// workspaceInitScriptRevision derives the ETag/If-Match value for a script's
// content, or the absent sentinel when there is none.
//
// Content-addressed, like internal/server's computeRevision for config.yaml
// and for the same reason spelled out there: a counter held in process memory
// resets on daemon restart and lets a stale If-Match match again.
func workspaceInitScriptRevision(data []byte, exists bool) string {
	if !exists {
		return WorkspaceInitScriptAbsentRevision
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// workspaceInitScriptContentIsAbsent reports whether content means "this
// workspace has no init.sh".
//
// An empty script is deliberately not a representable state: an existing
// empty file and an absent one are otherwise indistinguishable in effect
// (the wrapper runs bash on zero bytes either way as a no-op), so
// collapsing them also gives the envelope's `init_script: ""` and the
// CLI's unset one shared meaning.
//
// Whitespace is NOT trimmed before the check: a script of a single newline
// is still a script somebody wrote, and guessing at intent is how a write
// turns into a delete.
func workspaceInitScriptContentIsAbsent(content []byte) bool {
	return len(content) == 0
}

// validateWorkspaceInitScriptContent is the entry-point refusal: the size
// cap and the NUL byte, applied to CONTENT rather than to a request body.
//
// The cap is checked here, not only at the door, because the dedicated PUT
// is not the only way in: `boid workspace apply` also carries the script
// as spec.init_script inside a document read under the much larger
// workspaceBodyMaxBytes, so an oversized script could otherwise slip
// through that route. PutInitScript's MaxBytesReader still stays too — it
// is what stops an oversized body from being read into memory at all — but
// this check is what makes the limit a property of the script rather than
// of the request that carried it: every path that persists an init.sh
// goes through here (SetWorkspaceInitScript before it takes the lock, and
// validateWorkspaceApplyInitScript before the transaction opens).
//
// The NUL byte is rejected because it cannot survive delivery through the
// quoted heredoc the daemon wraps the script in, and a script containing
// one would fail every dispatch of the workspace from here on — refusing
// it here reports the problem to whoever wrote it, not to whoever
// triggers the next job.
//
// The shebang line is deliberately NOT validated: boid never execs this
// file, so a shebang has no effect at all (see the CLI's own note and
// docs/{ja,en}/guide/workspace-home.md).
func validateWorkspaceInitScriptContent(content []byte) error {
	if len(content) > workspaceInitScriptMaxBytes {
		return fmt.Errorf(
			"init.sh is %d bytes, over the %d-byte limit (the daemon buffers the whole script to hash it and to wrap it "+
				"in the init container's heredoc, and the limit is what keeps an accepted script restorable through "+
				"`boid workspace export` → `boid workspace apply`)",
			len(content), workspaceInitScriptMaxBytes)
	}
	if i := bytes.IndexByte(content, 0); i >= 0 {
		return fmt.Errorf(
			"init.sh contains a NUL byte at offset %d; a shell script must be text "+
				"(boid delivers it to the init container inside a quoted heredoc, which cannot carry one)", i)
	}
	return nil
}

// workspaceInitScriptStructuredDataMediaTypes are the media types this route
// refuses: the serialization formats whose arrival means the caller is sending
// something other than a shell script.
//
// Concretely, the mistake worth catching is a workspace yaml document or a
// boid.dev/v1 envelope PUT here instead of at /api/workspaces/{slug} or
// /api/workspaces/apply. It would be stored verbatim as the workspace's
// init.sh and handed to bash on the next dispatch. x-tar is on the list for
// the neighbouring confusion — the home-import route one path segment away
// takes exactly that.
var workspaceInitScriptStructuredDataMediaTypes = map[string]bool{
	"application/yaml":   true,
	"application/x-yaml": true,
	"text/yaml":          true,
	"text/x-yaml":        true,
	"application/json":   true,
	"text/json":          true,
	"application/xml":    true,
	"text/xml":           true,
	"text/html":          true,
	"application/x-tar":  true,
	"application/gzip":   true,
	"application/zip":    true,
}

// workspaceInitScriptMediaTypeError returns the reason to refuse this
// request's Content-Type, or "" when it is acceptable.
//
// A deny-list, not an allow-list: an allow-list would be a second refusal
// on top of validateWorkspaceInitScriptContent's own check, and one a
// perfectly valid script could hit (e.g. `curl --data-binary @init.sh`
// with no explicit header sends application/x-www-form-urlencoded). A
// deny-list still catches the yaml/json confusion without refusing shell
// scripts sent by anything other than the CLI. Unlike the home-import
// route (which destroys the workspace's HOME volume before reading the
// body, so a mislabelled upload there costs the operator their
// credentials), nothing here is destroyed before the body is read and
// validated in full — so an absent Content-Type is accepted too.
func workspaceInitScriptMediaTypeError(header string) string {
	if strings.TrimSpace(header) == "" {
		return ""
	}
	mediaType := header
	if i := strings.IndexByte(header, ';'); i >= 0 {
		mediaType = header[:i]
	}
	if !workspaceInitScriptStructuredDataMediaTypes[strings.TrimSpace(strings.ToLower(mediaType))] {
		return ""
	}
	return fmt.Sprintf(
		"Content-Type %q is a structured-data format, not a shell script — this endpoint stores the body verbatim as the "+
			"workspace's init.sh and runs it with bash. Send %s (or text/plain, or no Content-Type at all); "+
			"a workspace yaml document goes to PUT /api/workspaces/{slug} and a boid.dev/v1 envelope to POST /api/workspaces/apply",
		header, WorkspaceInitScriptContentType)
}

// GetInitScript handles GET /api/workspaces/{slug}/init-script: the
// workspace's init.sh verbatim, with an ETag.
//
// A workspace with no init.sh answers 404 — and still sets the ETag, to the
// absent sentinel. See WorkspaceInitScriptAbsentRevision for why the sentinel
// exists, and TestWorkspaceHandler_GetInitScript_AbsentIs404WithTheAbsentETag
// for why absence cannot be a 200 with an empty body.
func (h *WorkspaceHandler) GetInitScript(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	script, err := h.Service.GetWorkspaceInitScript(slug)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if script.Revision != "" {
		w.Header().Set("ETag", `"`+script.Revision+`"`)
	}
	if !script.Exists {
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"workspace %q has no init.sh (the daemon would keep it at %s); "+
				"write one with `boid workspace set-init-script %s -f <file>`", script.Slug, script.Path, script.Slug))
		return
	}
	w.Header().Set("Content-Type", WorkspaceInitScriptContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(script.Content)
}

// PutInitScript handles PUT /api/workspaces/{slug}/init-script[?force=true]:
// replace the workspace's init.sh with the request body, gated by If-Match
// unless force is set — the same convention PUT /api/workspaces/{slug} and
// POST /api/config already established (428 when If-Match is missing, 412 when
// it is stale).
//
// An EMPTY body clears the script rather than storing zero bytes; see
// workspaceInitScriptContentIsAbsent.
func (h *WorkspaceHandler) PutInitScript(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	if reason := workspaceInitScriptMediaTypeError(r.Header.Get("Content-Type")); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	body := http.MaxBytesReader(w, r.Body, workspaceInitScriptMaxBytes)
	content, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("request body unreadable or exceeds %d bytes: %v", workspaceInitScriptMaxBytes, err))
		return
	}

	force := r.URL.Query().Get("force") == "true"
	ifMatch := unquoteETag(r.Header.Get("If-Match"))

	result, err := h.Service.SetWorkspaceInitScript(slug, content, ifMatch, force)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Revision != "" {
		w.Header().Set("ETag", `"`+result.Revision+`"`)
	}
	writeJSON(w, http.StatusOK, result)
}

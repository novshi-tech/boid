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

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d), the API
// half: GET/PUT /api/workspaces/{slug}/init-script.
//
// # Why there is an endpoint for a file
//
// A workspace's init.sh is what installs the harness CLI and the toolchain
// into the workspace HOME volume. It is read by the daemon, from the daemon's
// own $XDG_CONFIG_HOME — which, once the daemon runs in a container, is inside
// a volume no editor on the host can reach. The dogfood run behind this plan
// doc put the scripts in with `podman cp`.
//
// This is the same shape `boid config get/set/apply/edit` took for config.yaml
// (docs/plans/volume-only-daemon.md §論点 f established the principle: a file
// the daemon owns is read and written through its HTTP API), and it follows
// the same conventions: raw bytes with a media type rather than a JSON
// envelope, an ETag on the read, If-Match on the write, ?force=true to opt out.
//
// # Why the routes hang off {slug}
//
// PR8 put the home migration at /{slug}/home/import with the note that "a
// future /{slug}/… surface has an obvious place to live". This is that
// surface. The target is a path parameter like every other per-workspace
// operation, and TestWorkspaceHandler_InitScriptRoutesDoNotShadowTheSlugRoutes
// pins that a workspace named "init-script" still works.

// The size budget for a workspace init.sh (D3, re-derived in PR9 codex
// round 2, Major 3).
//
// # Why there is a cap at all
//
// The daemon buffers the whole body — it hashes it into the completion marker
// and wraps it in a heredoc inside the init container's wrapper script
// (dispatcher.buildWorkspaceInitScript) — so an unbounded body is an unbounded
// allocation. Nothing downstream imposes a size: the script is delivered on
// the init container's STDIN, not in an argv, so Linux's 128 KiB
// per-argument ceiling (MAX_ARG_STRLEN) never applies.
//
// # Why it is 128 KiB and not the 1 MiB it started as
//
// The cap was originally set to 1 MiB purely for consistency with
// configBodyMaxBytes and workspaceBodyMaxBytes. That made the endpoint accept
// scripts it could not give back: `boid workspace export` is the endorsed
// backup path (docs/plans/volume-only-daemon.md §論点g), an export carries the
// script inside a yaml envelope, and `boid workspace apply` reads that
// envelope through the SAME 1 MiB workspaceBodyMaxBytes. A document containing
// a 1 MiB script is necessarily larger than 1 MiB, so every maximum-size
// script the API accepted produced an export that its own apply rejected with
// a 400 — discovered at restore time, which is the worst moment to learn a
// backup is not one.
//
// Of the two ways to close that gap, lowering this cap is the one with no
// cost. Raising the apply cap would raise the daemon's memory ceiling for
// every workspace document — the exact thing the caps exist to bound — to buy
// headroom for a file that is a few KB in practice: the reference
// implementation (docs/examples/workspace-home-init.sh) is under 5 KB and the
// five real scripts this plan doc migrates are 8.4 KB each. 128 KiB is still
// fifteen times the largest one that exists, and it leaves the body cap — the
// number an operator can actually observe, since it governs a request — as the
// single figure everything else is derived from. Anything approaching even
// this is a mistake (a tarball, a binary, the wrong file), and a refusal at
// the door beats the daemon holding it in memory and failing the next dispatch
// on it.
//
// The budget is asserted by TestWorkspaceInitScriptMaxBytes_IsDerivedFromTheApplyBodyCap
// and the whole path by TestWorkspaceInitScript_TheLargestAcceptedScriptSurvivesExportAndApply.
const (
	// workspaceInitScriptMaxBytes caps an init.sh request body.
	workspaceInitScriptMaxBytes = 128 << 10 // 128 KiB

	// workspaceInitScriptWorstCaseYAMLExpansion is how much larger a script
	// can get when yaml.Marshal writes it into an envelope's spec.init_script.
	//
	// MEASURED, not guessed — guessing is what produced the 1 MiB cap above.
	// TestWorkspaceInitScript_WorstCaseYAMLExpansionIsNotUnderstated runs the
	// shapes that stress each of yaml.v3's style choices at exactly
	// workspaceInitScriptMaxBytes and fails if any exceeds this number. The
	// worst of them is 5.0018x: a script of one-character lines, which the
	// literal block style re-emits at spec.init_script's 8-space indent, so
	// "a\n" (2 bytes) becomes "        a\n" (10). The next worst is 4.0018x
	// (every byte a C0 control, escaped as \xNN in a double-quoted scalar);
	// an ordinary shell script is 1.4724x. 6 rather than 5 leaves ~20% for a
	// yaml.v3 upgrade changing a style choice or an indent.
	workspaceInitScriptWorstCaseYAMLExpansion = 6

	// workspaceInitScriptEnvelopeAllowance is the room reserved in the same
	// document for everything that is NOT the script: apiVersion, kind,
	// metadata, and the workspace's own host_commands / env / allowed_domains
	// / extra_repos / container_image / capabilities / projects.
	//
	// 64 KiB against a measured 233 bytes for an empty workspace. The margin
	// is for the meta fields, which have no individual caps of their own —
	// only workspaceBodyMaxBytes bounds them, so a workspace whose env map
	// alone approaches the body cap cannot round-trip no matter what this
	// number is. That is a pre-existing property of the envelope and not one
	// init.sh introduces; what this allowance buys is that the SCRIPT is
	// never the reason an export stops fitting.
	//
	// It is a BUDGET, not a limit (PR9 codex round 3, Major 2). Nothing
	// refuses a workspace whose metadata exceeds it — the guarantee that an
	// export is applicable is enforced on the finished document instead, by
	// checkWorkspaceEnvelopeIsApplicable, which is the only place the two
	// halves are known together. See that function for why the enforcement
	// sits there rather than on a metadata field or on apply's own cap.
	workspaceInitScriptEnvelopeAllowance = 64 << 10
)

// WorkspaceInitScriptStore is the daemon-side capability the init-script
// surface needs: read, write and remove a workspace's init.sh at the path the
// DISPATCHER reads it from.
//
// Declared here, on the consumer side, and implemented by
// dispatcher.WorkspaceInitScriptStore — the same split
// WorkspaceHomeStore/WorkspaceHomeImporter use (論点 a-2, D2): the mechanism
// (where the file is, how it is published) lives with the code that reads it,
// the policy (If-Match, the cap, the NUL refusal, empty-means-absent) lives
// here with the workspace rows.
//
// A nil value turns the feature off — the route answers 501 and `apply` warns
// rather than silently dropping a spec.init_script — for the DI/test daemons
// that have no dispatcher wiring.
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
// An empty script is deliberately not a representable state. The two differ in
// exactly one observable way and it is a bad one: dispatch records
// sha256(zero bytes) in the completion marker for an existing empty file and
// "" for an absent one, so toggling between them re-runs init for no
// behavioural difference (the wrapper writes the bytes to a file and runs bash
// on it — zero bytes is a no-op either way). Collapsing them also gives the
// envelope's `init_script: ""` and the CLI's unset one shared meaning.
//
// Whitespace is NOT trimmed before the check: a script of a single newline is
// still a script somebody wrote, and guessing at intent is how a write turns
// into a delete.
func workspaceInitScriptContentIsAbsent(content []byte) bool {
	return len(content) == 0
}

// validateWorkspaceInitScriptContent is the entry-point refusal (D3): the size
// cap and the NUL byte, applied to CONTENT rather than to a request body.
//
// # Why the cap is checked here and not only at the door (PR9 codex round 3, Major 1)
//
// It used to live exclusively in PutInitScript's MaxBytesReader, which reads a
// request body — and the dedicated PUT is not the only way in. `boid workspace
// apply` carries the script as spec.init_script inside a document read under
// workspaceBodyMaxBytes, eight times larger, so an envelope of well under
// 1 MiB could deliver a 128 KiB + 1 script and get a 200. Every number the CLI
// help, both guides and the plan doc state as the limit was breakable by
// choosing the other route.
//
// The door check STAYS: MaxBytesReader is what stops an oversized body from
// being read into memory at all, which a check on already-read content cannot
// do. This is the one that makes the limit a property of the script rather
// than of the request that carried it — every path that persists an init.sh
// goes through here (SetWorkspaceInitScript before it takes the lock, and
// validateWorkspaceApplyInitScript before the transaction opens).
//
// # The NUL byte
//
// dispatcher.buildWorkspaceInitRequest is fail-closed on four conditions, and
// this is the only one besides the size that can be decided from the content
// alone at write time:
//
//   - a NUL byte cannot survive delivery through a quoted heredoc, and a
//     script containing one would fail EVERY dispatch of the workspace from
//     here on. Refusing it here reports it to whoever wrote it instead of to
//     whoever triggers the next job;
//   - the heredoc-delimiter collision is against a delimiter minted per run
//     from 32 hex characters of crypto/rand. There is nothing to compare
//     against at write time, and nothing to hit (2^-128). Not checked, by
//     design rather than by omission;
//   - an empty HomeID or HomeTarget are invariants of the init request the
//     daemon builds, nothing to do with the script's content.
//
// The shebang line is deliberately NOT validated. boid never execs this file,
// so a shebang has no effect at all — see the CLI's own note and
// docs/{ja,en}/guide/workspace-home.md. Rejecting (or requiring) one would
// suggest it matters.
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
// # Why a deny-list and not an allow-list (PR9 codex round 2, Minor 1)
//
// The entry-point contract for this endpoint's CONTENT is "a NUL byte and
// nothing else" (validateWorkspaceInitScriptContent). An allow-list of media
// types was a second refusal on top of that, and one a perfectly valid script
// could hit: `curl --data-binary @init.sh` with no explicit header sends
// application/x-www-form-urlencoded, so the most obvious way to drive this
// endpoint without the CLI was rejected for how the body was labelled rather
// than for anything about the body.
//
// A deny-list is the smallest implementation of what the check is actually
// worth: it still catches the yaml/json confusion, and it stops refusing shell
// scripts. The asymmetry with PR8's home-import route — which does use an
// allow-list, and refuses an absent type outright — is deliberate and is
// argued there: that route destroys the workspace's HOME volume BEFORE reading
// the body, so a mislabelled upload costs the operator their credentials.
// Nothing here is destroyed before the body has been read and validated in
// full.
//
// An ABSENT Content-Type is accepted for the same reason.
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

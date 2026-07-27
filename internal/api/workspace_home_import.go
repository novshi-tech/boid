package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// PR8 of docs/plans/workspace-home-volume-persistence.md (論点 f), API half:
// POST /api/workspaces/{slug}/home/import.
//
// # Why this is an endpoint at all (D1)
//
// The obvious shape for a one-off migration is a CLI that drives the engine
// directly, the way `boid reap` does. It is the wrong shape here, and
// dispatcher.Runner.ImportWorkspaceHome's own doc comment carries the full
// argument: the completion marker and the init flock both live inside the
// daemon's own persistent volume, and clearing the marker is HALF of 論点 f's
// acceptance condition. What the CLI has that the daemon does not is READ
// access to the host's legacy homes/<slug> tree — so the CLI supplies the
// bytes and the daemon does the work.
//
// # Streaming, not buffering
//
// The measured homes this exists to migrate run to 4.3GB. The request body is
// handed to the daemon as an io.Reader and travels from this socket to the
// extraction container's stdin without being materialized (see
// containerBackend.runWorkspaceHomeContainer's io.Copy). That is also why this
// is the one workspace endpoint with NO http.MaxBytesReader: the other bodies
// here are yaml documents of a few KB and are capped at 1 MiB
// (workspaceBodyMaxBytes), and applying that cap to this route would truncate
// every real migration into a corrupt archive.

// workspaceHomeImportContentType is the media type the migration body must
// carry.
//
// Required, and checked BEFORE anything else happens, because of the ordering
// the migration cannot avoid: the daemon replaces the home volume before it
// reads a single byte of the archive (see Runner.ImportWorkspaceHome for why
// the destruction has to come first — it is also the in-use check). A
// well-meaning `curl -d @workspace.yaml` against this path would therefore
// destroy a workspace's credentials and then fail at tar. A media type is a
// cheap door to close.
//
// # Required means required, including ABSENT (codex review of PR8, Blocker 1)
//
// The first implementation validated the header only when one was present, on
// the usual reading that an unset Content-Type is "unknown" rather than
// "wrong". That reading is right for a route whose worst case is a parse error
// and wrong for this one: `curl --data-binary @file` sends NO Content-Type by
// default, so the shape most likely to arrive by hand was exactly the shape
// that skipped the check — and skipping it here does not mean "guess the type",
// it means "delete the workspace's home and then find out". An absent
// declaration is therefore refused on the same footing as a wrong one. The cost
// of being strict is a client that must send one header; the cost of being
// lenient is measured in destroyed credentials.
const workspaceHomeImportContentType = "application/x-tar"

// workspaceHomeImportMediaTypeError returns the operator-facing reason to
// refuse this request's Content-Type, or "" when it declares the media type
// this route requires.
//
// Parameters are stripped (`application/x-tar; charset=binary`) and the type is
// matched case-insensitively, per RFC 9110 §8.3: a media type's type/subtype is
// case-insensitive and a client is free to append parameters. Hand-parsed
// rather than run through mime.ParseMediaType because the only thing that must
// be decided here is "is this the one string we accept" — a malformed header is
// refused with the same message as a merely wrong one, which is what an
// operator needs either way, and pulling in mime's error taxonomy would add a
// branch that says nothing new.
func workspaceHomeImportMediaTypeError(header string) string {
	if strings.TrimSpace(header) == "" {
		return fmt.Sprintf(
			"Content-Type is required and must be %s (the body is an uncompressed tar of the workspace home's contents). "+
				"It is refused rather than guessed because this route replaces the workspace's HOME volume BEFORE it reads "+
				"the body: a request carrying anything else would destroy the home and only then fail to extract",
			workspaceHomeImportContentType)
	}
	mediaType := header
	if i := strings.IndexByte(header, ';'); i >= 0 {
		mediaType = header[:i]
	}
	if strings.TrimSpace(strings.ToLower(mediaType)) != workspaceHomeImportContentType {
		return fmt.Sprintf(
			"Content-Type must be %s (the body is an uncompressed tar of the workspace home's contents), got %q",
			workspaceHomeImportContentType, header)
	}
	return ""
}

// WorkspaceHomeImporter is the daemon-side capability POST
// /api/workspaces/{slug}/home/import needs. *dispatcher.Runner is the only
// implementation.
//
// Declared here, on the consumer side, and typed in terms of
// dispatcher.WorkspaceHomeImportResult — a plain struct — for the same reason
// WorkspaceHomeStore is: this package imports internal/dispatcher and must not
// grow moby types (論点 a-2, D2), and a test needs to be able to stub it
// without an engine.
//
// A nil value is the feature's OFF switch, exactly as a nil
// WorkspaceHandler.Homes is: server/wire.go leaves it nil when there is no
// runner to serve it, and the handler answers 501 rather than panicking.
//
// # Why the removal guard is on THIS interface (codex round 2, Major 1)
//
// BeginWorkspaceHomeRemoval has nothing to do with importing, and it is here
// rather than on a field of its own for one reason: the exclusion only works if
// the object that registers a removal is the SAME object that registers a
// migration. Two fields could be wired to two different Runners — each with its
// own in-memory registry — and the result would exclude nothing while looking
// exactly like this. One interface, one field, one instance, and the compiler
// enforces it.
type WorkspaceHomeImporter interface {
	ImportWorkspaceHome(ctx context.Context, slug string, tarStream io.Reader) (dispatcher.WorkspaceHomeImportResult, error)

	// BeginWorkspaceHomeRemoval excludes a migration of slug's HOME volume until
	// the returned function is called. WorkspaceHandler.Remove brackets BOTH of
	// its deletions (the row, then the volume) with it.
	//
	// The only error it returns wraps dispatcher.ErrWorkspaceHomeBusy, so there
	// is exactly one thing to map (409) and the refusal always means "a migration
	// of this workspace is running; nothing has been changed".
	BeginWorkspaceHomeRemoval(slug string) (done func(), err error)
}

// WorkspaceHomeImportResponse is the response body for a completed migration.
//
// PreviousExisted and MarkerRemoved are both on the wire rather than folded
// into a single "ok", because they are 論点 f's two independent re-init legs
// and the CLI says something different when only one of them fired: with the
// marker still in place the re-init rests entirely on the new volume identity,
// which is true but worth telling an operator about.
type WorkspaceHomeImportResponse struct {
	Status string `json:"status"`

	// Slug is the NORMALIZED workspace slug the daemon migrated — not
	// necessarily the one in the URL, since an empty workspace id normalizes to
	// the default workspace.
	Slug string `json:"slug"`

	// Volume is the home volume that now holds the imported contents,
	// dockerres.WorkspaceHomeVolumeName(installID, slug) — the same name
	// `boid workspace show` reports and `docker volume rm` takes.
	Volume string `json:"volume"`

	// HomeID is the identity the NEW incarnation of that volume carries. It
	// differs from whatever the replaced one had; that difference is what makes
	// the completion marker stale and forces the re-init (論点 f leg 1).
	HomeID string `json:"home_id,omitempty"`

	// PreviousExisted reports whether there was a home volume to replace.
	// false means the workspace had never been dispatched into — the
	// init-on-first-dispatch contract, not an error.
	PreviousExisted bool `json:"previous_existed"`

	// MarkerRemoved / MarkerRemoveError report 論点 f leg 2, the deletion of
	// <dataHome>/homes-meta/<slug>.init.json. A failure is not fatal (leg 1
	// already re-armed the init) and is reported anyway.
	MarkerRemoved     bool   `json:"marker_removed"`
	MarkerRemoveError string `json:"marker_remove_error,omitempty"`

	// MigrationRecordRemoveError is non-empty when the migration succeeded but
	// the daemon could not delete the in-progress record it wrote before
	// starting (dispatcher's workspace_home_migration_sentinel.go).
	//
	// On the wire rather than only in the daemon log because the operator at the
	// terminal is the one who can act on it, and the daemon log is not where
	// somebody reading a CLI command's output looks. What it COSTS depends on the
	// field below.
	MigrationRecordRemoveError string `json:"migration_record_remove_error,omitempty"`

	// MigrationRecordDiscardsHome says whether that leftover record still
	// authorizes the next daemon start to DESTROY this home (codex round 4,
	// Blocker 1). Meaningful only alongside MigrationRecordRemoveError.
	//
	// false is the ordinary case and needs no action: the record was moved into
	// its "the home was rebuilt" phase before the deletion failed, so recovery
	// only deletes the file. true means it was not, and the consequence lands
	// later and looks unrelated when it does — the next daemon start discards
	// this workspace's freshly imported home and re-initializes it. The error
	// names the file, so deleting it before restarting saves re-doing a multi-GB
	// import.
	//
	// A separate field rather than something the CLI infers, because the CLI has
	// no way to tell the two apart: both arrive as "the record could not be
	// removed", and only the daemon knows how far it got before that.
	MigrationRecordDiscardsHome bool `json:"migration_record_discards_home,omitempty"`
}

// ImportHome handles POST /api/workspaces/{slug}/home/import: replace slug's
// HOME volume with the tar stream on the request body.
//
// Order of the checks is the contract, not a style choice. Everything that can
// refuse WITHOUT side effects happens first — media type, workspace row,
// importer wired — because the first thing the daemon does once past them is
// destroy the existing home volume.
func (h *WorkspaceHandler) ImportHome(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	// Unconditional, including the absent case — see
	// workspaceHomeImportMediaTypeError for why "declared wrong" and "not
	// declared at all" have to be refused on the same footing here.
	if reason := workspaceHomeImportMediaTypeError(r.Header.Get("Content-Type")); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	// The workspace has to exist. A migration into an unknown slug would mint a
	// HOME volume for a workspace nothing can dispatch into: it would then show
	// up in `boid gc`'s listing as an orphan, and `boid workspace remove` — the
	// obvious cleanup — 404s on a slug with no row. Refusing here is both the
	// earlier and the only recoverable answer.
	//
	// This check is not the whole story and cannot be (codex round 2, Major 1):
	// `boid workspace remove` can delete the row between here and the daemon's
	// first engine call, and this handler holds nothing that would stop it. The
	// daemon therefore asks the same question again while holding its migration
	// registration, from which point a removal IS refused — see
	// dispatcher.Runner.ConfirmWorkspaceExists. What this check buys is refusing
	// before a multi-GB body is read, which is worth having on its own.
	if _, err := h.Service.GetWorkspace(slug); err != nil {
		writeServiceError(w, err)
		return
	}

	if h.HomeImporter == nil {
		writeError(w, http.StatusNotImplemented,
			"this daemon cannot import a workspace home: no sandbox runner is wired into the workspace API "+
				"(a test/DI daemon, or one with no container backend)")
		return
	}

	// r.Body, unwrapped: see this file's header on why the 1 MiB cap the other
	// workspace endpoints use must not apply here.
	res, err := h.HomeImporter.ImportWorkspaceHome(r.Context(), slug, r.Body)
	if err != nil {
		// Two sentinels, one status. ErrWorkspaceHomeInUse is the ENGINE's answer
		// (a running container holds the volume); ErrWorkspaceHomeBusy is the
		// DAEMON's own (a dispatch of this workspace has resolved the home volume
		// and not yet created the container that would make it in-use, or another
		// migration is running — see dispatcher's workspaceHomeInFlight). They are
		// separate errors because the prose an operator reads differs, and one
		// status because what they do about it does not.
		//
		// 409 rather than 500: nothing was changed, and the next step is to wait
		// rather than to investigate. Same status the engine itself answers a busy
		// volume with, and the same one `workspace remove` surfaces for the
		// identical situation.
		if errors.Is(err, dispatcher.ErrWorkspaceHomeInUse) || errors.Is(err, dispatcher.ErrWorkspaceHomeBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		// 400, and neither of the two above: an archive with no entries is not a
		// transient to wait out and not a daemon fault to investigate — the
		// REQUEST is wrong and nothing was changed. The daemon checks it because
		// it is the only guard on the same side of the wire as the deletion it
		// prevents; see dispatcher's peekWorkspaceHomeTarHasEntries for why an
		// empty archive is uniquely destructive on a route that removes the volume
		// before reading the body.
		if errors.Is(err, dispatcher.ErrWorkspaceHomeImportEmpty) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// The daemon's own re-check of the workspace row (Major 1) returns the
		// service's error unchanged, wrapped — so the status it chose comes back
		// out here. errors.As rather than writeServiceError's type assertion
		// because it arrives wrapped, and err.Error() rather than status.Message
		// because the wrapper is what says WHERE the check failed and that nothing
		// was changed.
		var status *StatusError
		if errors.As(err, &status) {
			writeError(w, status.Code, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, WorkspaceHomeImportResponse{
		Status:                      "ok",
		Slug:                        res.Slug,
		Volume:                      res.Volume,
		HomeID:                      res.HomeID,
		PreviousExisted:             res.PreviousExisted,
		MarkerRemoved:               res.MarkerRemoved,
		MarkerRemoveError:           res.MarkerRemoveError,
		MigrationRecordRemoveError:  res.MigrationRecordRemoveError,
		MigrationRecordDiscardsHome: res.MigrationRecordDiscardsHome,
	})
}

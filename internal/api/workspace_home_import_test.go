package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// PR8 of docs/plans/workspace-home-volume-persistence.md (論点 f), API half:
// POST /api/workspaces/{slug}/home/import.
//
// The policy this layer owns is small and each piece has a reason: the
// workspace has to exist (a migration into a slug with no row would leave a
// volume for a workspace nobody can dispatch), the daemon has to actually be
// able to do it (a nil importer is the feature's off switch, matching how
// WorkspaceHandler.Homes gates the size/delete surface), and the two failures
// an operator can act on — a job holding the volume, an unknown workspace —
// have to arrive as distinguishable status codes rather than one 500.

// stubHomeImporter records what reached the daemon side and answers with
// whatever the test asked for.
type stubHomeImporter struct {
	slug     string
	received string
	result   dispatcher.WorkspaceHomeImportResult
	err      error

	// removalBegins counts BeginWorkspaceHomeRemoval calls and removalsOpen the
	// ones whose release has not run, so a test can pin BOTH halves of the
	// bracket `workspace remove` has to put around its two deletions (codex round
	// 2, Major 1). removalErr makes the guard refuse.
	removalBegins int
	removalsOpen  int
	removalErr    error
	// removalOpenDuringRemove is what removalsOpen was while Service.RemoveWorkspace
	// ran — the question that actually matters, since a guard released before the
	// row is deleted guards nothing.
	removalOpenDuringRemove int
}

func (s *stubHomeImporter) BeginWorkspaceHomeRemoval(string) (func(), error) {
	s.removalBegins++
	if s.removalErr != nil {
		return nil, s.removalErr
	}
	s.removalsOpen++
	return func() { s.removalsOpen-- }, nil
}

func (s *stubHomeImporter) ImportWorkspaceHome(_ context.Context, slug string, tarStream io.Reader) (dispatcher.WorkspaceHomeImportResult, error) {
	s.slug = slug
	// Read the stream even on the error path: the handler hands over the live
	// request body, and a stub that ignored it would hide a handler that had
	// already consumed or closed it.
	body, readErr := io.ReadAll(tarStream)
	s.received = string(body)
	if readErr != nil {
		return dispatcher.WorkspaceHomeImportResult{}, readErr
	}
	return s.result, s.err
}

func workspaceExistsService(slug string) *fakeWorkspaceService {
	return &fakeWorkspaceService{
		getFn: func(got string) (*WorkspaceDetail, error) {
			if got != slug {
				return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", got)}
			}
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}, Revision: "rev-1"}, nil
		},
	}
}

func TestWorkspaceHandler_ImportHome_StreamsTheArchiveAndReportsTheResult(t *testing.T) {
	imp := &stubHomeImporter{result: dispatcher.WorkspaceHomeImportResult{
		Slug:            "team-a",
		Volume:          "boid-ws-home-abcd1234-team-a",
		HomeID:          "new-identity",
		PreviousExisted: true,
		MarkerRemoved:   true,
	}}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar-bytes"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if imp.slug != "team-a" {
		t.Errorf("importer got slug %q, want team-a", imp.slug)
	}
	if imp.received != "tar-bytes" {
		t.Errorf("importer got body %q, want the request body verbatim", imp.received)
	}

	var resp WorkspaceHomeImportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Volume != "boid-ws-home-abcd1234-team-a" || resp.HomeID != "new-identity" {
		t.Errorf("response = %+v, want the daemon's own report", resp)
	}
	if !resp.PreviousExisted || !resp.MarkerRemoved {
		t.Errorf("response = %+v: the two 論点 f legs must be reported so the CLI can say whether both fired", resp)
	}
}

// TestWorkspaceHandler_ImportHome_ReportsWhatALeftoverRecordCosts is the wire
// half of codex round 4's Blocker 1.
//
// A migration that succeeded and then could not delete its own in-progress
// record has two very different outcomes, and only the daemon can tell them
// apart: either the record was moved out of the destructive phase first (the
// home is safe; the next start just deletes the file) or it was not (the next
// start discards a multi-GB import). The CLI prints opposite advice for the two,
// so both facts have to cross the wire — a response that carried only "the
// record could not be removed" would force the CLI to guess, and the safe guess
// is the one that sends every operator hunting for a file inside the daemon's
// state volume.
func TestWorkspaceHandler_ImportHome_ReportsWhatALeftoverRecordCosts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		discardsHome bool
	}{
		{"the window was closed first", false},
		{"the window is still open", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imp := &stubHomeImporter{result: dispatcher.WorkspaceHomeImportResult{
				Slug:                        "team-a",
				Volume:                      "boid-ws-home-abcd1234-team-a",
				PreviousExisted:             true,
				MarkerRemoved:               true,
				MigrationRecordRemoveError:  "remove /home/boid/.local/share/boid/homes-meta/team-a.migrating.json: permission denied",
				MigrationRecordDiscardsHome: tc.discardsHome,
			}}
			h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

			w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar-bytes"), nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var resp WorkspaceHomeImportResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.MigrationRecordRemoveError == "" {
				t.Errorf("response = %+v: the leftover record was not reported at all", resp)
			}
			if resp.MigrationRecordDiscardsHome != tc.discardsHome {
				t.Errorf("migration_record_discards_home = %v, want %v: the CLI decides between "+
					"\"nothing to do\" and \"delete this file before the next boid start\" on it alone",
					resp.MigrationRecordDiscardsHome, tc.discardsHome)
			}
		})
	}
}

// TestWorkspaceHandler_ImportHome_InUseIs409 is what makes D4's refusal
// actionable from the CLI: a job holding the volume is a "come back later",
// not a server error.
func TestWorkspaceHandler_ImportHome_InUseIs409(t *testing.T) {
	imp := &stubHomeImporter{err: fmt.Errorf("workspace home %q: refusing: %w", "team-a", dispatcher.ErrWorkspaceHomeInUse)}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar"), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "refusing") {
		t.Errorf("body %q loses the daemon's explanation", w.Body.String())
	}
}

// TestWorkspaceHandler_ImportHome_BusyIs409 is the same "come back later" as
// the case above, for the busy state the ENGINE cannot see: a dispatch of this
// workspace that has resolved its home volume and not yet created the container
// that would make the volume in-use (dispatcher.ErrWorkspaceHomeBusy — see
// dispatcher's workspaceHomeInFlight).
//
// It needs its own case because it is a different sentinel reaching the same
// branch, and the default for anything unrecognized here is 500 — which would
// tell an operator to investigate a daemon fault when what they have is a
// transient they should wait out.
func TestWorkspaceHandler_ImportHome_BusyIs409(t *testing.T) {
	imp := &stubHomeImporter{err: fmt.Errorf(
		"workspace home %q: refusing to migrate while 1 job(s) are being started in this workspace: %w",
		"team-a", dispatcher.ErrWorkspaceHomeBusy)}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar"), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "being started") {
		t.Errorf("body %q loses the daemon's explanation of what to wait for", w.Body.String())
	}
}

// TestWorkspaceHandler_ImportHome_EmptyArchiveIs400 is codex round 3's Blocker
// 2 at the status-code boundary.
//
// An archive with no entries is the ONE failure here that is neither a transient
// to wait out (409) nor a daemon fault to investigate (500): the request is
// wrong, the daemon changed nothing, and the fix is on the client's side — a
// --from that points at an empty directory, or at a symlink an older CLI failed
// to follow. Answering 500 would send an operator into the daemon log looking for
// a bug that is not there, and 409 would tell them to wait for something that
// will never finish.
func TestWorkspaceHandler_ImportHome_EmptyArchiveIs400(t *testing.T) {
	imp := &stubHomeImporter{err: fmt.Errorf(
		"workspace home %q: the uploaded archive contains no entries: %w", "team-a", dispatcher.ErrWorkspaceHomeImportEmpty)}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no entries") {
		t.Errorf("body %q loses the daemon's explanation of what was wrong with the request", w.Body.String())
	}
}

// TestWorkspaceHandler_ImportHome_UnknownWorkspaceIs404: the daemon must not
// mint a HOME volume for a slug that has no workspace row. A stray one would
// then show up in `boid gc` as an orphan the operator cannot remove with
// `boid workspace remove` (that 404s), i.e. exactly the state the orphan
// listing exists to warn about.
func TestWorkspaceHandler_ImportHome_UnknownWorkspaceIs404(t *testing.T) {
	imp := &stubHomeImporter{}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/nope/home/import", workspaceHomeImportContentType, []byte("tar"), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if imp.slug != "" {
		t.Error("the migration reached the daemon for a workspace with no row")
	}
}

// TestWorkspaceHandler_ImportHome_NoImporterWiredIs501 mirrors how
// WorkspaceHandler.Homes gates the size/delete surface: a nil field is the
// feature's off switch, and it has to say so rather than panic.
func TestWorkspaceHandler_ImportHome_NoImporterWiredIs501(t *testing.T) {
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a")}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar"), nil)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_ImportHome_RejectsAWrongContentType keeps a
// well-meaning `curl -d @file.yaml` from destroying a workspace home: the
// volume is replaced before the body is read, so "wrong payload" has to be
// caught at the door.
func TestWorkspaceHandler_ImportHome_RejectsAWrongContentType(t *testing.T) {
	imp := &stubHomeImporter{}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", "application/json", []byte("{}"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if imp.slug != "" {
		t.Error("a JSON body reached the migration, which would have destroyed the home volume first")
	}
}

// TestWorkspaceHandler_ImportHome_RequiresAContentType is the same door as the
// case above, on the hinge side it was left off.
//
// A request with NO Content-Type at all is the shape a hand-rolled `curl
// --data-binary @something` produces by default, and it is the one that gets
// past a check written as "if a media type was declared, it must be the right
// one". The consequence is not a rejected request but a destroyed workspace:
// the daemon replaces the home volume BEFORE it reads a byte of the body (that
// removal IS the in-use check — Runner.ImportWorkspaceHome's D4 note), so the
// migration deletes the credentials, then fails at tar, and answers 500 with
// the home already gone. Declaring the media type is the operator's only
// statement that the body is what this route destroys a home for.
func TestWorkspaceHandler_ImportHome_RequiresAContentType(t *testing.T) {
	imp := &stubHomeImporter{}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	// "" makes doWorkspaceRequest omit the header entirely, which is what a
	// client that never sets one sends.
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", "", []byte("not-a-tar"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if imp.slug != "" {
		t.Fatalf("a body with no Content-Type reached the migration (slug %q): the home volume is destroyed before the "+
			"archive is read, so this request would have wiped %q's credentials and then failed at tar", imp.slug, "team-a")
	}
	if !strings.Contains(w.Body.String(), workspaceHomeImportContentType) {
		t.Errorf("body %q does not tell the operator which media type to send", w.Body.String())
	}
}

// TestWorkspaceHandler_ImportHome_OtherFailuresAre500 covers the rest: an
// extraction that failed halfway is a real error, and its message is the only
// account of what the operator now has.
func TestWorkspaceHandler_ImportHome_OtherFailuresAre500(t *testing.T) {
	imp := &stubHomeImporter{err: errors.New("extract into \"boid-ws-home-x\": tar exited 2")}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar"), nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tar exited 2") {
		t.Errorf("body %q loses the reason", w.Body.String())
	}
}

// TestWorkspaceHandler_ImportHome_RunnerSatisfiesTheInterface is the type-level
// half of the wiring: server/wire.go assigns a *dispatcher.Runner into this
// field, and a signature drift on either side would otherwise only show up as
// a nil importer at run time.
func TestWorkspaceHandler_ImportHome_RunnerSatisfiesTheInterface(t *testing.T) {
	var _ WorkspaceHomeImporter = (*dispatcher.Runner)(nil)
}

// TestWorkspaceHandler_Remove_BracketsTheDeletionWithTheMigrationGuard is codex
// round 2's Major 1 at the layer that produces the race.
//
// Remove deletes the workspace ROW and then the home VOLUME. A migration of the
// same workspace that slips between (or around) those two ends up creating a
// fresh HOME volume for a row that is gone — full of the operator's harness
// credentials, unreachable by any dispatch, and no longer deletable by
// `workspace remove` itself, which 404s on the missing row. The exclusion has to
// be held across BOTH deletions, so the assertion is not "the guard was called"
// but "the guard was still open while the row was being deleted".
func TestWorkspaceHandler_Remove_BracketsTheDeletionWithTheMigrationGuard(t *testing.T) {
	imp := &stubHomeImporter{}
	svc := &fakeWorkspaceService{removeFn: func(string) error {
		imp.removalOpenDuringRemove = imp.removalsOpen
		return nil
	}}
	h := &WorkspaceHandler{Service: svc, HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if imp.removalBegins != 1 {
		t.Fatalf("BeginWorkspaceHomeRemoval was called %d times, want 1: `workspace remove` runs outside the exclusion "+
			"that keeps a concurrent `boid workspace import-home` from re-creating the HOME volume of the row it is deleting",
			imp.removalBegins)
	}
	if imp.removalOpenDuringRemove != 1 {
		t.Errorf("the removal guard was not open while the workspace row was being deleted (open = %d): a guard taken "+
			"and released around the wrong statement excludes nothing", imp.removalOpenDuringRemove)
	}
	if imp.removalsOpen != 0 {
		t.Errorf("%d removal registrations were left open after the handler returned: every later migration of this "+
			"workspace is refused for the lifetime of the daemon", imp.removalsOpen)
	}
}

// TestWorkspaceHandler_Remove_ConflictsWithAnInFlightMigration pins the status,
// and pins that the refusal happens BEFORE the row is deleted — which is what
// makes it free to retry.
func TestWorkspaceHandler_Remove_ConflictsWithAnInFlightMigration(t *testing.T) {
	imp := &stubHomeImporter{removalErr: fmt.Errorf("workspace home %q: a migration is running: %w", "team-a", dispatcher.ErrWorkspaceHomeBusy)}
	removed := false
	svc := &fakeWorkspaceService{removeFn: func(string) error { removed = true; return nil }}
	h := &WorkspaceHandler{Service: svc, HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if removed {
		t.Error("the workspace row was deleted despite the refusal: the whole value of refusing here is that the " +
			"operator still has the workspace they had")
	}
}

// TestWorkspaceHandler_Remove_WithoutAnImporterStillWorks keeps the guard from
// becoming a hard dependency: a daemon with no runner wired (test/DI wiring)
// cannot run a migration either, so there is nothing to exclude.
func TestWorkspaceHandler_Remove_WithoutAnImporterStillWorks(t *testing.T) {
	removed := false
	h := &WorkspaceHandler{Service: &fakeWorkspaceService{removeFn: func(string) error { removed = true; return nil }}}

	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !removed {
		t.Error("the workspace row was not deleted")
	}
}

// TestWorkspaceHandler_ImportHome_PropagatesTheDaemonsWorkspaceStatus is the
// other end of the same fix. The daemon re-checks the workspace row while
// holding its migration registration, and when that check fails it returns the
// service's own error — which carries the status. Flattening it to 500 would
// tell an operator to investigate a daemon fault when what happened is that the
// workspace was deleted while they were uploading.
func TestWorkspaceHandler_ImportHome_PropagatesTheDaemonsWorkspaceStatus(t *testing.T) {
	imp := &stubHomeImporter{err: fmt.Errorf("workspace home %q: the workspace is no longer registered: %w", "team-a",
		&StatusError{Code: http.StatusNotFound, Message: "workspace \"team-a\" not found"})}
	h := &WorkspaceHandler{Service: workspaceExistsService("team-a"), HomeImporter: imp}

	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/team-a/home/import", workspaceHomeImportContentType, []byte("tar-bytes"), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no longer registered") {
		t.Errorf("body %q drops the daemon's own explanation of where the check failed", w.Body.String())
	}
}

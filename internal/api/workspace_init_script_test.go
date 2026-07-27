package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d), the API
// half: GET/PUT /api/workspaces/{slug}/init-script.
//
// Two layers are exercised separately and both are needed:
//
//   - the HANDLER against a stub service — status codes, headers, media type,
//     the body cap;
//   - the SERVICE against the real dispatcher.WorkspaceInitScriptStore and a
//     real workspaces DB — If-Match, the NUL refusal, "empty means no script",
//     and the export/apply hydration.
//
// Splitting them the other way round (a handler test with the real store)
// would leave the service's own policy untested wherever the handler happens
// not to reach it, and the service is where every irreversible decision is.

// ---------------------------------------------------------------------------
// handler
// ---------------------------------------------------------------------------

func TestWorkspaceHandler_GetInitScript_ServesTheScriptWithAnETag(t *testing.T) {
	svc := &fakeWorkspaceService{
		getInitScriptFn: func(slug string) (*WorkspaceInitScript, error) {
			if slug != "team-a" {
				t.Errorf("slug = %q, want team-a", slug)
			}
			return &WorkspaceInitScript{
				Slug: slug, Exists: true, Content: []byte("echo hi\n"), Revision: "rev-abc",
				Path: "/home/boid/.config/boid/workspaces/team-a/init.sh",
			}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a/init-script", "", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "echo hi\n" {
		t.Errorf("body = %q, want the script verbatim", got)
	}
	if got := w.Header().Get("ETag"); got != `"rev-abc"` {
		t.Errorf("ETag = %q, want %q", got, `"rev-abc"`)
	}
	if got := w.Header().Get("Content-Type"); got != WorkspaceInitScriptContentType {
		t.Errorf("Content-Type = %q, want %q", got, WorkspaceInitScriptContentType)
	}
}

// TestWorkspaceHandler_GetInitScript_AbsentIs404WithTheAbsentETag pins the
// one unusual thing about this route.
//
// A workspace with no init.sh is not an error condition (the pass-through
// class), but it cannot be a 200 either — an empty 200 body is exactly what a
// workspace with a zero-byte script would produce, and the two have to stay
// distinguishable on the wire. It is a 404 that still carries an ETag, and
// that ETag is what `boid workspace set-init-script` sends as If-Match when it
// creates the first script for a workspace: the create is then guarded too, so
// a script that appeared between the CLI's read and its write is a 412 instead
// of a silent overwrite.
func TestWorkspaceHandler_GetInitScript_AbsentIs404WithTheAbsentETag(t *testing.T) {
	svc := &fakeWorkspaceService{
		getInitScriptFn: func(slug string) (*WorkspaceInitScript, error) {
			return &WorkspaceInitScript{Slug: slug, Exists: false, Revision: WorkspaceInitScriptAbsentRevision}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a/init-script", "", nil, nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `"`+WorkspaceInitScriptAbsentRevision+`"` {
		t.Errorf("ETag = %q, want the absent sentinel %q", got, WorkspaceInitScriptAbsentRevision)
	}
}

func TestWorkspaceHandler_PutInitScript_PassesIfMatchAndForceThrough(t *testing.T) {
	var gotIfMatch string
	var gotForce bool
	var gotContent []byte
	svc := &fakeWorkspaceService{
		setInitScriptFn: func(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error) {
			gotIfMatch, gotForce, gotContent = ifMatch, force, content
			return &WorkspaceInitScriptResult{Slug: slug, Action: WorkspaceInitScriptWritten, Revision: "rev-new", Bytes: len(content)}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a/init-script?force=true",
		WorkspaceInitScriptContentType, []byte("echo hi\n"), map[string]string{"If-Match": `"rev-old"`})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	// Quotes stripped by the same unquoteETag every other workspace endpoint
	// uses — the CLI round-trips the quoted header form verbatim.
	if gotIfMatch != "rev-old" {
		t.Errorf("ifMatch = %q, want %q (unquoted)", gotIfMatch, "rev-old")
	}
	if !gotForce {
		t.Error("force = false, want true (?force=true)")
	}
	if string(gotContent) != "echo hi\n" {
		t.Errorf("content = %q", gotContent)
	}
	var result WorkspaceInitScriptResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Action != WorkspaceInitScriptWritten {
		t.Errorf("action = %q, want %q", result.Action, WorkspaceInitScriptWritten)
	}
}

// TestWorkspaceHandler_PutInitScript_RejectsAStructuredDataMediaType keeps the
// one confusion the check exists for: a yaml/json body — a workspace document,
// or the envelope meant for `workspace apply` — stored verbatim as this
// workspace's shell script.
func TestWorkspaceHandler_PutInitScript_RejectsAStructuredDataMediaType(t *testing.T) {
	for _, ct := range []string{"application/yaml", "application/json", "text/yaml", "APPLICATION/JSON; charset=utf-8", "application/x-tar"} {
		svc := &fakeWorkspaceService{
			setInitScriptFn: func(string, []byte, string, bool) (*WorkspaceInitScriptResult, error) {
				t.Errorf("Content-Type %q: the service was reached with a rejected media type", ct)
				return nil, nil
			},
		}
		h := &WorkspaceHandler{Service: svc}
		w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a/init-script",
			ct, []byte("echo hi\n"), map[string]string{"If-Match": `"r"`})
		if w.Code != http.StatusBadRequest {
			t.Errorf("Content-Type %q: status = %d, want 400: %s", ct, w.Code, w.Body.String())
			continue
		}
		// The refusal has to say what to send instead — this is the only
		// rejection a caller can hit with a script that is otherwise valid.
		if !strings.Contains(w.Body.String(), WorkspaceInitScriptContentType) {
			t.Errorf("Content-Type %q: error %q does not name the type to send", ct, w.Body.String())
		}
	}
}

// TestWorkspaceHandler_PutInitScript_AcceptsAnythingThatIsNotStructuredData is
// PR9 codex round 2, Minor 1.
//
// The decided entry-point contract is that content is refused for a NUL byte
// and nothing else, and an allow-list of media types quietly added a second
// refusal that a valid script could hit. `curl --data-binary @init.sh` — the
// obvious way to drive this endpoint without the CLI — sends
// application/x-www-form-urlencoded by default and was refused, with a script
// the daemon would have accepted from any other caller.
//
// So the check is a deny-list now: exactly the structured-data types whose
// arrival means somebody is POSTing the wrong thing, and nothing else. That
// keeps what the check was actually for (see the test above) and drops the
// part that refused shell scripts for how they were labelled.
func TestWorkspaceHandler_PutInitScript_AcceptsAnythingThatIsNotStructuredData(t *testing.T) {
	types := []string{
		"", "text/plain", "text/plain; charset=utf-8", "text/x-shellscript",
		"application/x-sh", "APPLICATION/X-SHELLSCRIPT", "application/octet-stream",
		// `curl --data-binary @init.sh` with no -H, and `curl -F`.
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
	}
	for _, ct := range types {
		called := false
		svc := &fakeWorkspaceService{
			setInitScriptFn: func(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error) {
				called = true
				return &WorkspaceInitScriptResult{Slug: slug, Action: WorkspaceInitScriptWritten}, nil
			},
		}
		h := &WorkspaceHandler{Service: svc}
		w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a/init-script", ct,
			[]byte("echo hi\n"), map[string]string{"If-Match": `"r"`})
		if w.Code != http.StatusOK || !called {
			t.Errorf("Content-Type %q: status = %d called = %v, want 200/true: %s", ct, w.Code, called, w.Body.String())
		}
	}
}

// TestWorkspaceHandler_PutInitScript_RejectsAnOversizedBody pins D3's cap. The
// daemon buffers the whole script (to hash it and wrap it in a heredoc), so an
// unbounded body is an unbounded allocation.
func TestWorkspaceHandler_PutInitScript_RejectsAnOversizedBody(t *testing.T) {
	svc := &fakeWorkspaceService{
		setInitScriptFn: func(string, []byte, string, bool) (*WorkspaceInitScriptResult, error) {
			t.Fatal("the service must not be reached with an oversized body")
			return nil, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := make([]byte, workspaceInitScriptMaxBytes+1)
	for i := range body {
		body[i] = 'x'
	}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a/init-script",
		WorkspaceInitScriptContentType, body, map[string]string{"If-Match": `"r"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_InitScriptRoutesDoNotShadowTheSlugRoutes guards the
// chi routing decision (nested under {slug}, following PR8's /{slug}/home/…):
// a workspace can legitimately be called "init-script", and the per-workspace
// endpoints must keep working for it.
func TestWorkspaceHandler_InitScriptRoutesDoNotShadowTheSlugRoutes(t *testing.T) {
	var showSlug string
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			showSlug = slug
			return &WorkspaceDetail{Slug: slug, Revision: "rev-1"}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/init-script", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if showSlug != "init-script" {
		t.Errorf("Show received slug %q, want %q", showSlug, "init-script")
	}
}

// ---------------------------------------------------------------------------
// service (real store, real workspaces DB)
// ---------------------------------------------------------------------------

// newInitScriptTestService wires a ProjectAppService with the REAL
// dispatcher-side store, rooted at an isolated $XDG_CONFIG_HOME for this test,
// plus a workspaces DB carrying the given slugs.
//
// The real store rather than a stub, deliberately: the property most worth
// testing here is that the API's writes land where the dispatcher reads, and a
// stub is precisely the thing that cannot tell you that.
func newInitScriptTestService(t *testing.T, slugs ...string) (*ProjectAppService, string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	for _, slug := range slugs {
		if err := repo.Save(slug, &orchestrator.WorkspaceMeta{}); err != nil {
			t.Fatalf("Save(%q): %v", slug, err)
		}
	}
	return &ProjectAppService{
		Projects:       &stubProjectRepository{},
		Meta:           &stubProjectMetaStore{},
		Workspaces:     repo,
		WorkspacesConn: d.Conn,
		InitScripts:    dispatcher.WorkspaceInitScriptStore{},
	}, configDir
}

// initScriptPathFor is the path the DISPATCHER will read, derived
// independently of the store under test so an assertion about "the file
// landed" is not simply the store agreeing with itself.
func initScriptPathFor(configDir, slug string) string {
	return filepath.Join(configDir, "boid", "workspaces", slug, "init.sh")
}

func TestSetWorkspaceInitScript_WritesWhereTheDispatcherReads(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")

	cur, err := svc.GetWorkspaceInitScript("team-a")
	if err != nil {
		t.Fatalf("GetWorkspaceInitScript: %v", err)
	}
	if cur.Exists {
		t.Fatal("a fresh workspace already reports an init.sh")
	}
	if cur.Revision != WorkspaceInitScriptAbsentRevision {
		t.Errorf("revision of an absent script = %q, want %q", cur.Revision, WorkspaceInitScriptAbsentRevision)
	}

	res, err := svc.SetWorkspaceInitScript("team-a", []byte("echo hi\n"), cur.Revision, false)
	if err != nil {
		t.Fatalf("SetWorkspaceInitScript: %v", err)
	}
	if res.Action != WorkspaceInitScriptWritten {
		t.Errorf("action = %q, want %q", res.Action, WorkspaceInitScriptWritten)
	}

	got, err := os.ReadFile(initScriptPathFor(configDir, "team-a"))
	if err != nil {
		t.Fatalf("the script did not land at the path the dispatcher reads: %v", err)
	}
	if string(got) != "echo hi\n" {
		t.Errorf("on-disk init.sh = %q, want %q", got, "echo hi\n")
	}
	if res.Path != initScriptPathFor(configDir, "team-a") {
		t.Errorf("reported path = %q, want %q", res.Path, initScriptPathFor(configDir, "team-a"))
	}
}

// TestSetWorkspaceInitScript_IfMatchContract pins the same 428/412 pair every
// other whole-document endpoint in this daemon uses (`boid config apply`,
// `boid workspace edit`).
func TestSetWorkspaceInitScript_IfMatchContract(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a")

	// Missing If-Match: 428, and nothing written.
	_, err := svc.SetWorkspaceInitScript("team-a", []byte("v1\n"), "", false)
	assertStatusError(t, err, http.StatusPreconditionRequired)
	if cur, _ := svc.GetWorkspaceInitScript("team-a"); cur.Exists {
		t.Fatal("a rejected 428 write still created the script")
	}

	// Correct If-Match (the absent sentinel): accepted.
	res, err := svc.SetWorkspaceInitScript("team-a", []byte("v1\n"), WorkspaceInitScriptAbsentRevision, false)
	if err != nil {
		t.Fatalf("SetWorkspaceInitScript: %v", err)
	}
	rev1 := res.Revision
	if rev1 == WorkspaceInitScriptAbsentRevision || rev1 == "" {
		t.Fatalf("revision after a write = %q, want a content-derived value", rev1)
	}

	// Stale If-Match (the value from before that write): 412, content intact.
	_, err = svc.SetWorkspaceInitScript("team-a", []byte("v2\n"), WorkspaceInitScriptAbsentRevision, false)
	assertStatusError(t, err, http.StatusPreconditionFailed)
	cur, err := svc.GetWorkspaceInitScript("team-a")
	if err != nil {
		t.Fatalf("GetWorkspaceInitScript: %v", err)
	}
	if string(cur.Content) != "v1\n" {
		t.Errorf("content after a rejected 412 write = %q, want the untouched %q", cur.Content, "v1\n")
	}

	// force skips the check entirely.
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("v2\n"), "", true); err != nil {
		t.Fatalf("SetWorkspaceInitScript(force): %v", err)
	}
	cur, _ = svc.GetWorkspaceInitScript("team-a")
	if string(cur.Content) != "v2\n" {
		t.Errorf("content after a forced write = %q, want %q", cur.Content, "v2\n")
	}
}

// TestSetWorkspaceInitScript_RejectsNULBytesAtTheDoor pins D3.
//
// dispatcher.buildWorkspaceInitRequest already refuses a script containing a
// NUL — it cannot be delivered through a heredoc — but that refusal fires at
// DISPATCH time, which is a job failure for whoever happens to trigger the
// next one, arbitrarily long after the write. The entry point is where it can
// still be reported to the person who caused it.
//
// The other three fail-closed conditions in that function are deliberately NOT
// mirrored here: the heredoc-delimiter collision is against a delimiter minted
// per run from 32 hex characters of crypto/rand, so there is nothing to check
// against at write time (and nothing to hit — 2^-128); the empty HomeID and
// HomeTarget checks are internal invariants of the init request, unrelated to
// the script's content.
func TestSetWorkspaceInitScript_RejectsNULBytesAtTheDoor(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")

	_, err := svc.SetWorkspaceInitScript("team-a", []byte("echo hi\x00\n"), WorkspaceInitScriptAbsentRevision, false)
	assertStatusError(t, err, http.StatusBadRequest)
	if !strings.Contains(err.Error(), "NUL") {
		t.Errorf("error = %q, want it to name the NUL byte", err)
	}
	if _, statErr := os.Stat(initScriptPathFor(configDir, "team-a")); statErr == nil {
		t.Error("a script rejected for a NUL byte was written anyway")
	}
}

// TestSetWorkspaceInitScript_EmptyContentClearsTheScript pins the decision
// that an empty script is not a representable state.
//
// The two would otherwise differ in exactly one observable way and it is a bad
// one: dispatch hashes an existing empty file into the completion marker
// (scriptSHA256Hex of zero bytes) where an absent one records "", so switching
// between them re-runs init for no behavioural difference — the wrapper writes
// the bytes to a file and runs bash on it either way. Collapsing them also
// gives the envelope's `init_script: ""` and the CLI's unset the same meaning.
func TestSetWorkspaceInitScript_EmptyContentClearsTheScript(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")

	res, err := svc.SetWorkspaceInitScript("team-a", []byte("echo hi\n"), WorkspaceInitScriptAbsentRevision, false)
	if err != nil {
		t.Fatalf("SetWorkspaceInitScript: %v", err)
	}

	res, err = svc.SetWorkspaceInitScript("team-a", nil, res.Revision, false)
	if err != nil {
		t.Fatalf("SetWorkspaceInitScript(empty): %v", err)
	}
	if res.Action != WorkspaceInitScriptCleared {
		t.Errorf("action = %q, want %q", res.Action, WorkspaceInitScriptCleared)
	}
	if res.Revision != WorkspaceInitScriptAbsentRevision {
		t.Errorf("revision after a clear = %q, want %q", res.Revision, WorkspaceInitScriptAbsentRevision)
	}
	if _, statErr := os.Stat(initScriptPathFor(configDir, "team-a")); !os.IsNotExist(statErr) {
		t.Errorf("init.sh still on disk after a clear (stat err = %v)", statErr)
	}

	// Clearing an already-absent script is a no-op, not an error: `apply`ing
	// the same backup twice must not start failing on the second run.
	res, err = svc.SetWorkspaceInitScript("team-a", nil, WorkspaceInitScriptAbsentRevision, false)
	if err != nil {
		t.Fatalf("SetWorkspaceInitScript(empty, already absent): %v", err)
	}
	if res.Action != WorkspaceInitScriptUnchanged {
		t.Errorf("action = %q, want %q", res.Action, WorkspaceInitScriptUnchanged)
	}
}

// TestWorkspaceInitScript_UnknownWorkspaceIs404 keeps a typo'd slug from
// minting a config directory for a workspace that does not exist — the same
// reasoning PR8's home-import route gives for refusing an unknown slug before
// it creates a volume nothing can dispatch into.
func TestWorkspaceInitScript_UnknownWorkspaceIs404(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")

	_, err := svc.GetWorkspaceInitScript("nope")
	assertStatusError(t, err, http.StatusNotFound)

	_, err = svc.SetWorkspaceInitScript("nope", []byte("echo hi\n"), WorkspaceInitScriptAbsentRevision, false)
	assertStatusError(t, err, http.StatusNotFound)

	if _, statErr := os.Stat(filepath.Join(configDir, "boid", "workspaces", "nope")); statErr == nil {
		t.Error("a rejected write created a config directory for an unknown workspace")
	}
}

// TestExportWorkspaceEnvelopes_CarriesTheInitScript is the export half of D1's
// round trip.
func TestExportWorkspaceEnvelopes_CarriesTheInitScript(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a")
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("#!/bin/sh\necho hi\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("SetWorkspaceInitScript: %v", err)
	}

	data, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err != nil {
		t.Fatalf("ExportWorkspaceEnvelopes: %v", err)
	}
	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("decode export: %v\n%s", err, data)
	}
	if got := docs[0].Envelope.Spec.InitScript; got != "#!/bin/sh\necho hi\n" {
		t.Errorf("exported init_script = %q, want the script verbatim\n%s", got, data)
	}
}

// TestApplyWorkspace_HydratesTheInitScript is the apply half: the three
// presence cases, each against a workspace that already has a script.
func TestApplyWorkspace_HydratesTheInitScript(t *testing.T) {
	tests := []struct {
		name       string
		present    bool
		script     string
		wantAction string
		wantOnDisk string
		wantExists bool
	}{
		{name: "absent key leaves it alone", present: false, wantAction: "", wantOnDisk: "original\n", wantExists: true},
		{name: "explicit empty clears it", present: true, script: "", wantAction: WorkspaceInitScriptCleared, wantExists: false},
		{name: "content replaces it", present: true, script: "replaced\n", wantAction: WorkspaceInitScriptWritten, wantOnDisk: "replaced\n", wantExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, configDir := newInitScriptTestService(t, "team-a")
			if _, err := svc.SetWorkspaceInitScript("team-a", []byte("original\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
				t.Fatalf("seed: %v", err)
			}

			fields := map[string]bool{}
			if tt.present {
				fields["init_script"] = true
			}
			apply := envelopeApplyDoc("team-a", fields, orchestrator.WorkspaceEnvelopeSpec{InitScript: tt.script})
			result, err := svc.ApplyWorkspace(apply, false)
			if err != nil {
				t.Fatalf("ApplyWorkspace: %v", err)
			}
			if result.InitScriptAction != tt.wantAction {
				t.Errorf("InitScriptAction = %q, want %q", result.InitScriptAction, tt.wantAction)
			}

			data, statErr := os.ReadFile(initScriptPathFor(configDir, "team-a"))
			if tt.wantExists {
				if statErr != nil {
					t.Fatalf("init.sh missing after apply: %v", statErr)
				}
				if string(data) != tt.wantOnDisk {
					t.Errorf("on-disk init.sh = %q, want %q", data, tt.wantOnDisk)
				}
			} else if !os.IsNotExist(statErr) {
				t.Errorf("init.sh still present after a clearing apply (err = %v, content = %q)", statErr, data)
			}
		})
	}
}

// TestApplyWorkspace_RejectsAnUnusableInitScriptBeforeTouchingTheDB pins where
// the content check sits relative to the transaction.
//
// spec.init_script is written AFTER the commit (a file and a transaction
// cannot commit together — see applyWorkspaceInitScript), so a check that ran
// at write time would report "the metadata was applied, the script was not"
// for a document that was invalid from the start and could have been refused
// outright. It runs with the other pre-transaction validations instead, which
// makes the refusal a plain 400 with nothing touched — and keeps it out of the
// post-commit error path, where a wrapped *StatusError would surface as a 500.
//
// dryRun is checked too, for the reason ApplyWorkspace's own doc comment gives
// about host_commands: a dry run must hit the same validation a real apply
// would, or it reports success on a document the real apply rejects.
func TestApplyWorkspace_RejectsAnUnusableInitScriptBeforeTouchingTheDB(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		svc, configDir := newInitScriptTestService(t)

		apply := envelopeApplyDoc("brand-new", map[string]bool{"init_script": true},
			orchestrator.WorkspaceEnvelopeSpec{InitScript: "echo hi\x00\n"})
		_, err := svc.ApplyWorkspace(apply, dryRun)
		assertStatusError(t, err, http.StatusBadRequest)
		if !strings.Contains(err.Error(), "NUL") {
			t.Errorf("dryRun=%v: error = %q, want it to name the NUL byte", dryRun, err)
		}

		// Nothing committed: the workspace row was never created...
		if _, _, loadErr := svc.Workspaces.LoadWithRevision("brand-new"); loadErr == nil {
			t.Errorf("dryRun=%v: a refused apply created the workspace row", dryRun)
		}
		// ...and no config directory was minted for it either.
		if _, statErr := os.Stat(filepath.Join(configDir, "boid", "workspaces", "brand-new")); statErr == nil {
			t.Errorf("dryRun=%v: a refused apply created the workspace's config directory", dryRun)
		}
	}
}

// TestApplyWorkspace_DryRunNeverTouchesTheInitScript pins the same property
// the DB half already has: --dry-run runs every check and writes nothing.
func TestApplyWorkspace_DryRunNeverTouchesTheInitScript(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("original\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"init_script": true},
		orchestrator.WorkspaceEnvelopeSpec{InitScript: "replaced\n"})
	result, err := svc.ApplyWorkspace(apply, true)
	if err != nil {
		t.Fatalf("ApplyWorkspace(dryRun): %v", err)
	}
	if result.InitScriptAction != WorkspaceInitScriptWritten {
		t.Errorf("InitScriptAction = %q, want %q (a dry run still reports what it WOULD do)", result.InitScriptAction, WorkspaceInitScriptWritten)
	}
	data, err := os.ReadFile(initScriptPathFor(configDir, "team-a"))
	if err != nil {
		t.Fatalf("read init.sh: %v", err)
	}
	if string(data) != "original\n" {
		t.Errorf("dry run modified init.sh: %q", data)
	}
}

// TestApplyWorkspace_NoInitScriptStoreLeavesTheFieldAlone covers the DI/test
// daemon: a service with no store wired must not fail an apply that carries
// the field, it just cannot honor it — and says so rather than reporting a
// write that never happened.
func TestApplyWorkspace_NoInitScriptStoreLeavesTheFieldAlone(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a")
	svc.InitScripts = nil

	apply := envelopeApplyDoc("team-a", map[string]bool{"init_script": true},
		orchestrator.WorkspaceEnvelopeSpec{InitScript: "replaced\n"})
	result, err := svc.ApplyWorkspace(apply, false)
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if result.InitScriptAction != "" {
		t.Errorf("InitScriptAction = %q, want empty", result.InitScriptAction)
	}
	if len(result.Warnings) == 0 {
		t.Error("apply silently ignored spec.init_script with no store wired; want a warning")
	}
}

// ---------------------------------------------------------------------------
// remove (PR9 codex round 2, Major 1)
// ---------------------------------------------------------------------------

// TestWorkspaceHandler_Remove_DeletesTheInitScriptSoARecreatedSlugStartsClean
// is the scenario the review named.
//
// `workspace remove` deleted the row and the HOME volume and left the script
// behind, while dispatch resolves the script from the SLUG alone. Re-creating
// a workspace with the same name — the ordinary way to start over — therefore
// silently inherited the removed workspace's init.sh and ran it against the
// new, empty HOME volume. The operator could not even clean it up first:
// `unset-init-script` 404s once the row is gone.
func TestWorkspaceHandler_Remove_DeletesTheInitScriptSoARecreatedSlugStartsClean(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("script A\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(initScriptPathFor(configDir, "team-a")); !os.IsNotExist(err) {
		t.Errorf("init.sh outlived the workspace it belongs to (stat err = %v)", err)
	}

	// The consequence, spelled out: an empty workspace re-created under the
	// same slug must not come back holding the old script.
	if err := svc.Workspaces.Create("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("re-create the workspace: %v", err)
	}
	cur, err := svc.GetWorkspaceInitScript("team-a")
	if err != nil {
		t.Fatalf("GetWorkspaceInitScript: %v", err)
	}
	if cur.Exists {
		t.Errorf("a re-created workspace inherited the removed one's init.sh: %q", cur.Content)
	}
}

// TestWorkspaceHandler_Remove_ReportsWhatHappenedToTheInitScript keeps the
// deletion out of the silent column.
//
// The home volume's deletion is already reported field by field because a
// part-completed remove is a state an operator has to act on. The script is
// the same kind of leftover — one that a later workspace of the same name
// would inherit — so "removed" on its own is not an answer.
func TestWorkspaceHandler_Remove_ReportsWhatHappenedToTheInitScript(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a", "team-b")
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("script A\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := &WorkspaceHandler{Service: svc}

	var withScript WorkspaceRemoveResponse
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &withScript); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !withScript.InitScriptDeleted {
		t.Errorf("init_script_deleted = false for a workspace that had one: %s", w.Body.String())
	}
	if withScript.InitScriptDeleteError != "" {
		t.Errorf("init_script_delete_error = %q, want empty", withScript.InitScriptDeleteError)
	}

	// A workspace that never had one reports false with no error — nothing was
	// left behind, which is not the same claim as "a deletion succeeded".
	var withoutScript WorkspaceRemoveResponse
	w = doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-b", "", nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &withoutScript); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if withoutScript.InitScriptDeleted {
		t.Error("init_script_deleted = true for a workspace that never had one")
	}
	if withoutScript.InitScriptDeleteError != "" {
		t.Errorf("init_script_delete_error = %q, want empty", withoutScript.InitScriptDeleteError)
	}
}

// TestWorkspaceHandler_Remove_AFailedInitScriptDeletionIsReportedNotFatal
// pins the best-effort contract, the same one the home volume's deletion
// already has.
//
// The row is gone by the time this runs, so turning a failed unlink into a 500
// would tell the operator the remove failed when its irreversible half
// succeeded — and `workspace remove` cannot be re-run to finish the job
// (it 404s on the missing row). The response says what is still there instead,
// with the path in the message, so the leftover can be deleted by hand.
func TestWorkspaceHandler_Remove_AFailedInitScriptDeletionIsReportedNotFatal(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")

	// A directory where the file belongs: os.Remove refuses a non-empty one,
	// which is the closest reproducible stand-in for the real failures (a
	// read-only or otherwise unwritable daemon config volume).
	scriptPath := initScriptPathFor(configDir, "team-a")
	if err := os.MkdirAll(filepath.Join(scriptPath, "occupied"), 0o700); err != nil {
		t.Fatalf("stage the failure: %v", err)
	}

	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the row removal already committed): %s", w.Code, w.Body.String())
	}
	var resp WorkspaceRemoveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.InitScriptDeleteError == "" {
		t.Fatalf("a failed init.sh deletion was not reported: %s", w.Body.String())
	}
	if !strings.Contains(resp.InitScriptDeleteError, scriptPath) {
		t.Errorf("init_script_delete_error = %q, want it to name %q so the leftover can be found", resp.InitScriptDeleteError, scriptPath)
	}
	if resp.InitScriptDeleted {
		t.Error("init_script_deleted = true alongside a deletion error")
	}
	// The row is gone regardless — that is what makes this best-effort rather
	// than a rollback.
	if _, _, err := svc.Workspaces.LoadWithRevision("team-a"); err == nil {
		t.Error("the workspace row survived; this endpoint is not transactional and must not pretend to be")
	}
}

// TestDeleteWorkspaceInitScript_RefusesTheReservedDefaultWorkspace mirrors
// deleteWorkspaceHome's own guard, and for the same reason it gives: the
// row-level refusal in RemoveWorkspace already makes this unreachable through
// the handler, but the destructive half gets an independent guard anyway so a
// future caller — or a bug in the row-level check — cannot take out the
// default workspace's script.
func TestDeleteWorkspaceInitScript_RefusesTheReservedDefaultWorkspace(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, orchestrator.DefaultWorkspaceSlug)
	if _, err := svc.SetWorkspaceInitScript(orchestrator.DefaultWorkspaceSlug, []byte("script D\n"),
		WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deleted, err := deleteWorkspaceInitScript(svc.InitScripts, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("deleteWorkspaceInitScript: %v", err)
	}
	if deleted {
		t.Error("the reserved default workspace's init.sh was deleted")
	}
	if _, statErr := os.Stat(initScriptPathFor(configDir, orchestrator.DefaultWorkspaceSlug)); statErr != nil {
		t.Errorf("the default workspace's init.sh is gone: %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// concurrency (PR9 codex round 2, Major 2)
// ---------------------------------------------------------------------------

// removeRowOnFirstLoad removes slug's row the first time it is asked to load
// it, and answers that first call as if the row were still there.
//
// It reproduces exactly one interleaving and does so deterministically: the
// existence check runs, a `workspace remove` lands, and only then does the
// write take the mutex. A goroutine racing a real RemoveWorkspace would be
// testing the scheduler.
type removeRowOnFirstLoad struct {
	WorkspaceStore
	slug string
	once sync.Once
}

func (s *removeRowOnFirstLoad) LoadWithRevision(slug string) (*orchestrator.WorkspaceMeta, string, error) {
	meta, revision, err := s.WorkspaceStore.LoadWithRevision(slug)
	if slug == s.slug && err == nil {
		s.once.Do(func() { _ = s.WorkspaceStore.Remove(slug) })
	}
	return meta, revision, err
}

// TestSetWorkspaceInitScript_RefusesAWorkspaceRemovedBeforeItTookTheLock is
// Major 2.
//
// The existence check sat OUTSIDE the mutex, so a `workspace remove` landing
// between it and the lock left the write to proceed against a workspace that
// no longer existed: a config directory and an init.sh for a slug with no row,
// unreachable through this very API (every route 404s on the missing row) and
// waiting to be inherited by the next workspace created with that name.
// ?force=true made it certain rather than merely likely, since the revision
// comparison that might otherwise have failed is skipped.
//
// The fix is PR8's: re-confirm the row after taking the exclusion, so the
// answer cannot go stale before it is used (see
// dispatcher.Runner.ConfirmWorkspaceExists, asked under the migration
// registration for the same reason). Adding Major 1's cleanup on its own does
// not cover this — the cleanup runs first and this write re-creates the file
// behind it.
func TestSetWorkspaceInitScript_RefusesAWorkspaceRemovedBeforeItTookTheLock(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			svc, configDir := newInitScriptTestService(t, "team-a")
			svc.Workspaces = &removeRowOnFirstLoad{WorkspaceStore: svc.Workspaces, slug: "team-a"}

			ifMatch := ""
			if !force {
				ifMatch = WorkspaceInitScriptAbsentRevision
			}
			_, err := svc.SetWorkspaceInitScript("team-a", []byte("echo hi\n"), ifMatch, force)
			assertStatusError(t, err, http.StatusNotFound)

			if _, statErr := os.Stat(initScriptPathFor(configDir, "team-a")); statErr == nil {
				t.Error("an init.sh was written for a workspace that no longer has a row")
			}
		})
	}
}

// pauseOnSecondLoad parks the caller inside its SECOND load of slug, so a test
// can look at what else can happen at that moment.
type pauseOnSecondLoad struct {
	WorkspaceStore
	slug string
	// loads is only ever touched by the goroutine running the write under
	// test — the two loads are sequential within one SetWorkspaceInitScript
	// call, and nothing else in this service reaches LoadWithRevision.
	loads  int
	inside chan struct{}
	resume chan struct{}
}

func (s *pauseOnSecondLoad) LoadWithRevision(slug string) (*orchestrator.WorkspaceMeta, string, error) {
	meta, revision, err := s.WorkspaceStore.LoadWithRevision(slug)
	if slug == s.slug {
		s.loads++
		if s.loads == 2 {
			close(s.inside)
			<-s.resume
		}
	}
	return meta, revision, err
}

// TestSetWorkspaceInitScript_ChecksTheWorkspaceRowWhileHoldingTheServiceMutex
// pins WHERE Major 2's re-check sits, which is the whole of the fix.
//
// A second check placed anywhere outside s.mu would satisfy "the row is
// verified twice" and still leave the window open: RemoveWorkspace could land
// between it and the lock exactly as it did between the first check and the
// lock. What makes the answer usable is that a removal is excluded from the
// moment it is asked until the write is done.
//
// So this asserts the exclusion directly: while the write is parked on its
// second row check, a concurrent RemoveWorkspace must not be able to complete.
// It also covers the check's existence — with no second check the write never
// reaches the park and the test times out.
func TestSetWorkspaceInitScript_ChecksTheWorkspaceRowWhileHoldingTheServiceMutex(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a")
	probe := &pauseOnSecondLoad{
		WorkspaceStore: svc.Workspaces,
		slug:           "team-a",
		inside:         make(chan struct{}),
		resume:         make(chan struct{}),
	}
	svc.Workspaces = probe

	setDone := make(chan error, 1)
	go func() {
		_, err := svc.SetWorkspaceInitScript("team-a", []byte("echo hi\n"), WorkspaceInitScriptAbsentRevision, false)
		setDone <- err
	}()

	select {
	case <-probe.inside:
	case <-time.After(10 * time.Second):
		close(probe.resume)
		t.Fatal("SetWorkspaceInitScript never re-checked the workspace row: the only check ran before the mutex was taken, " +
			"so a concurrent `workspace remove` can still land between it and the write")
	}

	removeDone := make(chan error, 1)
	go func() {
		_, err := svc.RemoveWorkspace("team-a")
		removeDone <- err
	}()

	select {
	case err := <-removeDone:
		close(probe.resume)
		<-setDone
		t.Fatalf("a concurrent RemoveWorkspace completed (err = %v) while the write was at its row check — "+
			"the check is not inside the service mutex, so its answer can still go stale before the write uses it", err)
	case <-time.After(200 * time.Millisecond):
		// Blocked on s.mu. That is the property.
	}

	close(probe.resume)
	if err := <-setDone; err != nil {
		t.Fatalf("SetWorkspaceInitScript: %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("RemoveWorkspace (released once the write finished): %v", err)
	}
}

// ---------------------------------------------------------------------------
// size budget (PR9 codex round 2, Major 3)
// ---------------------------------------------------------------------------

// worstCaseYAMLInitScript builds an n-byte script in the shape that costs the
// most to encode as YAML: one-character lines.
//
// Each such line is re-emitted at the block scalar's 8-space indent, so two
// input bytes ("a\n") become ten ("        a\n") — the 5x that
// workspaceInitScriptWorstCaseYAMLExpansion is sized against. n must be even.
func worstCaseYAMLInitScript(n int) []byte {
	return bytes.Repeat([]byte("a\n"), n/2)
}

// TestWorkspaceInitScript_TheLargestAcceptedScriptSurvivesExportAndApply is
// Major 3's headline: whatever this API accepts must be restorable through the
// backup path the docs endorse.
//
// It did not hold. The PUT cap and the apply body cap were the same 1 MiB, and
// an export of a 1 MiB script is a document strictly larger than 1 MiB — so a
// script the daemon accepted, exported cleanly, and reported no problem with
// came back as a 400 from `boid workspace apply`. The failure surfaced at
// restore time, which is the worst possible moment to learn a backup is not
// one.
//
// The workspace carries a FULL WorkspaceMeta sized to the envelope allowance
// (PR9 codex round 3, Major 2), not the empty one this test started with: with
// an empty meta the only thing being measured is the script's share, and the
// allowance — the other half of the budget — could be any number at all
// without the test noticing.
func TestWorkspaceInitScript_TheLargestAcceptedScriptSurvivesExportAndApply(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")
	if err := svc.Workspaces.Save("team-a", allowanceSizedWorkspaceMeta(t)); err != nil {
		t.Fatalf("seed the meta: %v", err)
	}

	script := worstCaseYAMLInitScript(workspaceInitScriptMaxBytes)
	if len(script) != workspaceInitScriptMaxBytes {
		t.Fatalf("test script is %d bytes, want exactly the cap (%d)", len(script), workspaceInitScriptMaxBytes)
	}
	if _, err := svc.SetWorkspaceInitScript("team-a", script, WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("a script of exactly the cap was refused: %v", err)
	}

	data, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err != nil {
		t.Fatalf("ExportWorkspaceEnvelopes: %v", err)
	}
	// What `boid workspace apply` actually POSTs is one re-marshaled document
	// per workspace, not the file — so that is what has to fit.
	raws, err := orchestrator.SplitWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("SplitWorkspaceEnvelopeDocuments: %v", err)
	}
	for i, raw := range raws {
		t.Logf("document %d: %d bytes for a %d-byte script (%.4fx), export marshal %d, apply body cap %d",
			i, len(raw), len(script), float64(len(raw))/float64(len(script)), len(data), workspaceBodyMaxBytes)
		if len(raw) > workspaceBodyMaxBytes {
			t.Fatalf("document %d is %d bytes, over the %d-byte apply body cap: an accepted script's own export cannot be applied",
				i, len(raw), workspaceBodyMaxBytes)
		}
		// checkWorkspaceEnvelopeIsApplicable measures what EXPORT marshaled;
		// what apply reads is this re-marshal. The refusal is only as good as
		// those two agreeing, so the gap between them is pinned here rather
		// than assumed (PR9 codex round 3, Major 2).
		if len(raw) > len(data) {
			t.Errorf("document %d re-marshals to %d bytes from a %d-byte export: the export-side size check measures the wrong number",
				i, len(raw), len(data))
		}
	}

	// And it really does come back, byte for byte.
	if _, err := svc.SetWorkspaceInitScript("team-a", nil, "", true); err != nil {
		t.Fatalf("clear before restore: %v", err)
	}
	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(raws[0])
	if err != nil {
		t.Fatalf("decode the exported document: %v", err)
	}
	if _, err := svc.ApplyWorkspace(docs[0], false); err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	restored, err := os.ReadFile(initScriptPathFor(configDir, "team-a"))
	if err != nil {
		t.Fatalf("read the restored init.sh: %v", err)
	}
	if !bytes.Equal(restored, script) {
		t.Errorf("restored init.sh differs from the exported one (%d bytes vs %d)", len(restored), len(script))
	}
}

// TestExportWorkspaceEnvelopes_MetadataStringsSurviveExportAndApply is PR9
// codex round 4, Major 1: D13-b's guarantee ("a successful export can always be
// applied") for the strings that are NOT the init script.
//
// Round 2 fixed the yaml.v3 block-scalar defect for spec.init_script alone,
// which left every other string in the document carrying it: a workspace whose
// env value begins with a TAB exported a document that is not valid YAML, and
// one that begins with a NEWLINE exported a document that restores a DIFFERENT
// value. Both were reported as a successful export — the only check on the
// finished document was its size.
//
// The end-to-end path is what is asserted, not just the marshal:
// export → split (what the CLI POSTs) → decode → apply → read the row back.
func TestExportWorkspaceEnvelopes_MetadataStringsSurviveExportAndApply(t *testing.T) {
	probes := map[string]string{
		"leading tab":         "\tfirst\nsecond\n",
		"leading blank line":  "\nfirst\nsecond\n",
		"leading space":       " first\nsecond\n",
		"blank line only":     "\n",
		"tab then blank line": "\t\nsecond\n",
	}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			svc, _ := newInitScriptTestService(t, "team-a")
			meta := &orchestrator.WorkspaceMeta{
				// The value slot, the KEY slot (a mapping key is emitted by a
				// different branch of the emitter than a mapping value), a
				// sequence entry, and a plain scalar.
				Env:            map[string]string{"PROBE": probe, probe: "probe-key"},
				ExtraRepos:     []string{probe},
				AllowedDomains: []string{probe},
				ContainerImage: probe,
			}
			if err := svc.Workspaces.Save("team-a", meta); err != nil {
				t.Fatalf("seed the meta: %v", err)
			}
			if _, err := svc.SetWorkspaceInitScript("team-a", []byte("echo ok\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
				t.Fatalf("seed the script: %v", err)
			}

			data, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
			if err != nil {
				t.Fatalf("ExportWorkspaceEnvelopes: %v", err)
			}
			raws, err := orchestrator.SplitWorkspaceEnvelopeDocuments(data)
			if err != nil {
				t.Fatalf("the exported file cannot be split for apply: %v\ndocument:\n%s", err, data)
			}
			docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(raws[0])
			if err != nil {
				t.Fatalf("the exported document cannot be applied: %v\ndocument:\n%s", err, raws[0])
			}
			if _, err := svc.ApplyWorkspace(docs[0], false); err != nil {
				t.Fatalf("ApplyWorkspace: %v", err)
			}

			restored, _, err := svc.Workspaces.LoadWithRevision("team-a")
			if err != nil {
				t.Fatalf("reload the applied workspace: %v", err)
			}
			if got := restored.Env["PROBE"]; got != probe {
				t.Errorf("env value restored as %q, want %q\ndocument:\n%s", got, probe, raws[0])
			}
			if got := restored.Env[probe]; got != "probe-key" {
				t.Errorf("env KEY did not survive: restored keys %q\ndocument:\n%s", keysOfEnv(restored.Env), raws[0])
			}
			if len(restored.ExtraRepos) != 1 || restored.ExtraRepos[0] != probe {
				t.Errorf("extra_repos restored as %q, want [%q]\ndocument:\n%s", restored.ExtraRepos, probe, raws[0])
			}
			if len(restored.AllowedDomains) != 1 || restored.AllowedDomains[0] != probe {
				t.Errorf("allowed_domains restored as %q, want [%q]\ndocument:\n%s", restored.AllowedDomains, probe, raws[0])
			}
			if restored.ContainerImage != probe {
				t.Errorf("container_image restored as %q, want %q\ndocument:\n%s", restored.ContainerImage, probe, raws[0])
			}
		})
	}
}

func keysOfEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestWorkspaceInitScriptMaxBytes_IsDerivedFromTheApplyBodyCap makes the
// cap's arithmetic checkable rather than a comment.
//
// The three constants are a budget: the largest script this API accepts, times
// the most yaml can inflate it, plus room for the rest of the envelope, must
// still fit in the body cap the apply endpoint enforces. Changing any one of
// them without the others is the mistake this catches.
func TestWorkspaceInitScriptMaxBytes_IsDerivedFromTheApplyBodyCap(t *testing.T) {
	budget := workspaceInitScriptMaxBytes*workspaceInitScriptWorstCaseYAMLExpansion + workspaceInitScriptEnvelopeAllowance
	if budget > workspaceBodyMaxBytes {
		t.Fatalf("worst-case export of a max-size init.sh is %d bytes (%d x %d + %d), over the %d-byte apply body cap",
			budget, workspaceInitScriptMaxBytes, workspaceInitScriptWorstCaseYAMLExpansion,
			workspaceInitScriptEnvelopeAllowance, workspaceBodyMaxBytes)
	}
	t.Logf("budget %d of %d bytes (%d bytes spare)", budget, workspaceBodyMaxBytes, workspaceBodyMaxBytes-budget)
}

// TestWorkspaceInitScript_WorstCaseYAMLExpansionIsNotUnderstated measures the
// constant the budget above is built on, against the shapes that stress each
// of yaml.v3's style choices.
//
// A guessed expansion factor is what made the original cap wrong. This one is
// measured at the cap's own size — the fixed envelope overhead amortizes away
// there, so what is left is the per-byte cost the budget actually depends on.
func TestWorkspaceInitScript_WorstCaseYAMLExpansionIsNotUnderstated(t *testing.T) {
	const n = workspaceInitScriptMaxBytes
	shapes := map[string][]byte{
		// Block scalar, one character per line: every line pays the 8-space
		// indent. The worst shape found.
		"one-character lines": worstCaseYAMLInitScript(n),
		// Double-quoted, every byte escaped as \xNN.
		"control bytes": bytes.Repeat([]byte{0x01}, n),
		"del bytes":     bytes.Repeat([]byte{0x7f}, n),
		// Double-quoted with fold opportunities at every space.
		"control bytes with spaces": bytes.Repeat([]byte{0x01, ' '}, n/2),
		// !!binary (base64).
		"invalid utf-8": bytes.Repeat([]byte{0xff}, n),
		// The quoted-style fallback this PR added for a leading blank.
		"leading tab":        append([]byte("\t"), worstCaseYAMLInitScript(n-2)...),
		"leading blank line": append([]byte("\n"), worstCaseYAMLInitScript(n-2)...),
		// An ordinary script, for scale.
		"ordinary shell": bytes.Repeat([]byte("echo hello world\n"), n/17),
	}
	for name, script := range shapes {
		envelope := orchestrator.NewWorkspaceEnvelopeFromMeta("team-a", &orchestrator.WorkspaceMeta{}, nil, string(script))
		data, err := yaml.Marshal(envelope)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		ratio := float64(len(data)) / float64(len(script))
		t.Logf("%-26s %7d bytes -> %8d bytes (%.4fx)", name, len(script), len(data), ratio)
		if ratio > workspaceInitScriptWorstCaseYAMLExpansion {
			t.Errorf("%s expands %.4fx, over the %dx the cap is budgeted against",
				name, ratio, workspaceInitScriptWorstCaseYAMLExpansion)
		}
	}
}

// TestWorkspaceHandler_PutInitScript_AcceptsExactlyTheCap is the other half of
// the boundary: the limit itself is accepted, and the door is what refuses the
// byte past it (TestWorkspaceHandler_PutInitScript_RejectsAnOversizedBody).
func TestWorkspaceHandler_PutInitScript_AcceptsExactlyTheCap(t *testing.T) {
	called := false
	svc := &fakeWorkspaceService{
		setInitScriptFn: func(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error) {
			called = true
			if len(content) != workspaceInitScriptMaxBytes {
				t.Errorf("service received %d bytes, want %d", len(content), workspaceInitScriptMaxBytes)
			}
			return &WorkspaceInitScriptResult{Slug: slug, Action: WorkspaceInitScriptWritten, Bytes: len(content)}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a/init-script",
		WorkspaceInitScriptContentType, worstCaseYAMLInitScript(workspaceInitScriptMaxBytes),
		map[string]string{"If-Match": `"r"`})
	if w.Code != http.StatusOK || !called {
		t.Fatalf("status = %d called = %v, want 200/true: %s", w.Code, called, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// the cap is enforced on every route into the store
// (PR9 codex round 3, Major 1)
// ---------------------------------------------------------------------------

// overCapInitScript returns a script one byte past workspaceInitScriptMaxBytes,
// in the shape an envelope carries CHEAPLY — an ordinary shell script encodes
// at about 1.47x, so a document carrying this one is nowhere near the 1 MiB
// apply body cap.
//
// That combination is Major 1's whole scenario: the cap was enforced only by
// the dedicated PUT's MaxBytesReader, so a script the PUT refused arrived
// through `workspace apply` inside a perfectly legal document and was stored.
func overCapInitScript() []byte {
	const line = "echo hello world\n"
	s := bytes.Repeat([]byte(line), workspaceInitScriptMaxBytes/len(line)+1)
	return s[:workspaceInitScriptMaxBytes+1]
}

// TestWorkspaceHandler_Apply_CannotBypassTheInitScriptCap is the reported
// scenario end to end: a document that is itself well within the apply body
// cap, carrying a spec.init_script one byte over the init.sh cap.
//
// Before the fix this answered 200 and left a 128 KiB + 1 script on the daemon
// — the number the CLI help, both guides and the plan doc all state as the
// limit, broken by a route that never checked it.
func TestWorkspaceHandler_Apply_CannotBypassTheInitScriptCap(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")
	script := overCapInitScript()

	envelope := orchestrator.NewWorkspaceEnvelopeFromMeta("team-a", &orchestrator.WorkspaceMeta{}, nil, string(script))
	doc, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The premise: nothing about the REQUEST is oversized. If this ever fails
	// the test has stopped exercising the bypass and is just re-testing the
	// body cap.
	if len(doc) > workspaceBodyMaxBytes {
		t.Fatalf("the test document is %d bytes, over the %d-byte body cap: it would be refused for its size, not for the script's",
			len(doc), workspaceBodyMaxBytes)
	}
	t.Logf("document %d bytes for a %d-byte script (%.4fx), body cap %d",
		len(doc), len(script), float64(len(doc))/float64(len(script)), workspaceBodyMaxBytes)

	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", doc, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: apply stored a %d-byte init.sh, %d bytes over the documented cap\n%s",
			w.Code, len(script), len(script)-workspaceInitScriptMaxBytes, w.Body.String())
	}
	if _, statErr := os.Stat(initScriptPathFor(configDir, "team-a")); !os.IsNotExist(statErr) {
		t.Errorf("a refused apply still wrote the script (stat err = %v)", statErr)
	}
}

// TestWorkspaceInitScript_TheCapIsEnforcedBySetAndApplyAlike puts the refusal
// where BOTH write paths pass through, rather than at one door.
//
// The dedicated PUT's MaxBytesReader is a door check and stays — it is what
// keeps an oversized body from being read into memory at all. It cannot be the
// only check, because ApplyWorkspace's content never goes through it: the
// script arrives as a field of a document read under a different, larger cap.
func TestWorkspaceInitScript_TheCapIsEnforcedBySetAndApplyAlike(t *testing.T) {
	script := overCapInitScript()

	t.Run("set", func(t *testing.T) {
		svc, configDir := newInitScriptTestService(t, "team-a")
		_, err := svc.SetWorkspaceInitScript("team-a", script, WorkspaceInitScriptAbsentRevision, false)
		assertStatusError(t, err, http.StatusBadRequest)
		if _, statErr := os.Stat(initScriptPathFor(configDir, "team-a")); !os.IsNotExist(statErr) {
			t.Errorf("a refused write still created the script (stat err = %v)", statErr)
		}
	})

	// dryRun too, for the reason ApplyWorkspace's own doc comment gives: a dry
	// run that skips a check reports success on a document the real apply
	// rejects.
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply/dryRun=%v", dryRun), func(t *testing.T) {
			svc, configDir := newInitScriptTestService(t)
			apply := envelopeApplyDoc("brand-new", map[string]bool{"init_script": true},
				orchestrator.WorkspaceEnvelopeSpec{InitScript: string(script)})
			_, err := svc.ApplyWorkspace(apply, dryRun)
			assertStatusError(t, err, http.StatusBadRequest)
			if !strings.Contains(err.Error(), fmt.Sprint(workspaceInitScriptMaxBytes)) {
				t.Errorf("error = %q, want it to state the %d-byte cap", err, workspaceInitScriptMaxBytes)
			}
			// Refused BEFORE the transaction, like every other content check
			// on this field: no row, no config directory.
			if _, _, loadErr := svc.Workspaces.LoadWithRevision("brand-new"); loadErr == nil {
				t.Error("a refused apply created the workspace row")
			}
			if _, statErr := os.Stat(filepath.Join(configDir, "boid", "workspaces", "brand-new")); statErr == nil {
				t.Error("a refused apply created the workspace's config directory")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// export emits nothing its own apply would reject
// (PR9 codex round 3, Major 2)
// ---------------------------------------------------------------------------

// allowanceSizedWorkspaceMeta builds a WorkspaceMeta whose envelope encoding
// lands just under workspaceInitScriptEnvelopeAllowance, with every meta field
// populated.
//
// The boundary test used an EMPTY meta, which is why the allowance could be
// wrong without anything noticing: 233 bytes of overhead leaves so much slack
// that the script's share was the only thing being measured.
func allowanceSizedWorkspaceMeta(t *testing.T) *orchestrator.WorkspaceMeta {
	t.Helper()
	meta := &orchestrator.WorkspaceMeta{
		Env:            map[string]string{"TOOLCHAIN_MANIFEST": strings.Repeat("x", 60<<10)},
		HostCommands:   []string{"gh", "docker"},
		AllowedDomains: []string{"proxy.golang.org", ".pypi.org"},
		ExtraRepos:     []string{"https://github.com/novshi-tech/boid-kits"},
		ContainerImage: "ghcr.io/novshi-tech/boid-sandbox:latest",
		Capabilities:   orchestrator.Capabilities{Docker: &orchestrator.DockerCapability{}},
	}
	encoded, err := yaml.Marshal(orchestrator.NewWorkspaceEnvelopeFromMeta("team-a", meta,
		[]orchestrator.WorkspaceEnvelopeProject{{Name: "api", URL: "https://github.com/novshi-tech/api"}}, ""))
	if err != nil {
		t.Fatalf("marshal the meta-only envelope: %v", err)
	}
	if len(encoded) > workspaceInitScriptEnvelopeAllowance {
		t.Fatalf("the test meta encodes to %d bytes, over the %d-byte allowance it is meant to sit inside",
			len(encoded), workspaceInitScriptEnvelopeAllowance)
	}
	t.Logf("meta-only envelope %d bytes of the %d-byte allowance", len(encoded), workspaceInitScriptEnvelopeAllowance)
	return meta
}

// TestExportWorkspaceEnvelopes_RefusesADocumentItsOwnApplyWouldReject is
// Major 2.
//
// The 64 KiB allowance was arithmetic only: nothing bounded the metadata, so a
// workspace with a large env plus a cap-sized script exported a document over
// the 1 MiB apply body cap. The export reported success. `boid workspace
// apply` of that file answered 400 while reading the body — discovered at
// restore time, on a file the operator was told was a backup.
func TestExportWorkspaceEnvelopes_RefusesADocumentItsOwnApplyWouldReject(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a")
	if err := svc.Workspaces.Save("team-a", &orchestrator.WorkspaceMeta{
		Env: map[string]string{"HUGE": strings.Repeat("x", 400<<10)},
	}); err != nil {
		t.Fatalf("seed the meta: %v", err)
	}
	if _, err := svc.SetWorkspaceInitScript("team-a", worstCaseYAMLInitScript(workspaceInitScriptMaxBytes),
		WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed the script: %v", err)
	}

	data, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err == nil {
		t.Fatalf("export emitted a %d-byte document for a %d-byte apply body cap: applying this 'backup' is a 400",
			len(data), workspaceBodyMaxBytes)
	}
	// 409, the code export already answers for the other documents it refuses
	// to emit because they would not survive their own apply (an ambiguous or
	// ID-colliding project name).
	assertStatusError(t, err, http.StatusConflict)
	for _, want := range []string{"team-a", fmt.Sprint(workspaceBodyMaxBytes)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestExportWorkspaceEnvelopes_AllOrNothingWhenOneWorkspaceIsTooLarge pins the
// blast radius of the refusal above: `boid workspace export --all` fails
// rather than quietly dropping the workspace it cannot represent.
//
// Emitting the others and omitting one would produce a file that restores
// PART of the install without saying so, which is the same class of problem
// as the unrestorable document — just harder to notice.
func TestExportWorkspaceEnvelopes_AllOrNothingWhenOneWorkspaceIsTooLarge(t *testing.T) {
	svc, _ := newInitScriptTestService(t, "team-a", "team-b")
	if err := svc.Workspaces.Save("team-b", &orchestrator.WorkspaceMeta{
		Env: map[string]string{"HUGE": strings.Repeat("x", 1100<<10)},
	}); err != nil {
		t.Fatalf("seed the meta: %v", err)
	}
	if _, err := svc.ExportWorkspaceEnvelopes(nil); err == nil {
		t.Fatal("export --all succeeded while one of its documents cannot be applied")
	} else if !strings.Contains(err.Error(), "team-b") {
		t.Errorf("error = %q, want it to name the workspace at fault", err)
	}
}

// ---------------------------------------------------------------------------
// remove's critical section (PR9 codex round 3, Major 3)
// ---------------------------------------------------------------------------

// pauseInsideHomeDeletion parks DELETE /api/workspaces/{slug} in the middle of
// its home-volume deletion — the step that runs after the row is gone — so a
// test can act in that window deterministically.
type pauseInsideHomeDeletion struct {
	WorkspaceHomeStore
	inside chan struct{}
	resume chan struct{}
}

func (s *pauseInsideHomeDeletion) Remove(ctx context.Context, slug string) (dispatcher.WorkspaceHomeVolume, bool, error) {
	close(s.inside)
	<-s.resume
	return s.WorkspaceHomeStore.Remove(ctx, slug)
}

// pauseInsideInitScriptRemoval parks the init.sh deletion ITSELF — the step
// whose position inside the row's critical section is the property under test —
// so a test can act while it is half-done.
//
// Only the FIRST Remove pauses: the same store method is what `apply` uses to
// clear a script, and a test that deadlocked on the operation it is waiting for
// would report a hang instead of a result.
type pauseInsideInitScriptRemoval struct {
	WorkspaceInitScriptStore
	once   sync.Once
	inside chan struct{}
	resume chan struct{}
}

func (s *pauseInsideInitScriptRemoval) Remove(slug string) (bool, error) {
	s.once.Do(func() {
		close(s.inside)
		<-s.resume
	})
	return s.WorkspaceInitScriptStore.Remove(slug)
}

// TestWorkspaceHandler_Remove_DeletesTheInitScriptInsideTheRowsCriticalSection
// is Major 3 (PR9 codex round 3), pinned at the position it is about (round 4,
// Minor).
//
// The row removal took the service mutex and RELEASED it, and the init.sh
// deletion took it again in a separate call. Between the two, an apply could
// upsert the same slug, write a new script and answer 200 — and the removal
// then deleted that script. The final state is a workspace row that exists
// with the script of a successful apply missing, which nothing reports and
// nothing can distinguish from "this workspace never had one".
//
// The property is not "the deletion happens" but "an apply cannot OVERLAP it":
// entirely before it (row and script both go, correct) or entirely after it (a
// fresh workspace keeps its script, correct).
//
// # Why the pause is inside the deletion and not after it
//
// Round 3's version stopped the request in the HOME-volume deletion, which runs
// after ProjectAppService.RemoveWorkspace has already returned — so the mutex
// was released either way by the time the test acted, and moving the init.sh
// deletion out from under the mutex (anywhere before that return) left the test
// green. Stopping inside WorkspaceInitScriptStore.Remove is the only window in
// which "the deletion is holding the lock" and "the deletion is not holding the
// lock" look different from the outside.
func TestWorkspaceHandler_Remove_DeletesTheInitScriptInsideTheRowsCriticalSection(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("script A\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	scripts := &pauseInsideInitScriptRemoval{
		WorkspaceInitScriptStore: svc.InitScripts,
		inside:                   make(chan struct{}),
		resume:                   make(chan struct{}),
	}
	svc.InitScripts = scripts

	h := &WorkspaceHandler{Service: svc, Homes: newStubHomeStore(map[string]int64{"team-a": 4096})}
	removeDone := make(chan int, 1)
	go func() {
		removeDone <- doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil).Code
	}()

	select {
	case <-scripts.inside:
	case <-time.After(10 * time.Second):
		t.Fatal("DELETE never reached the init.sh deletion")
	}

	// The removal is now mid-deletion of the script. An apply of the same slug
	// must not be able to run to completion here: whatever it writes would be
	// deleted by the unlink that is still in flight, and it would answer 200.
	apply := envelopeApplyDoc("team-a", map[string]bool{"init_script": true},
		orchestrator.WorkspaceEnvelopeSpec{InitScript: "script B\n"})
	applyStarted := make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		close(applyStarted)
		_, err := svc.ApplyWorkspace(apply, false)
		applyDone <- err
	}()
	<-applyStarted

	select {
	case err := <-applyDone:
		t.Fatalf("an apply ran to completion (err=%v) while `workspace remove` was still deleting the init.sh — "+
			"the deletion is outside the row's critical section, so it can unlink a script a committed apply has already answered 200 for", err)
	case <-time.After(500 * time.Millisecond):
	}

	close(scripts.resume)
	if err := <-applyDone; err != nil {
		t.Fatalf("the apply that was blocked by the removal: %v", err)
	}
	if code := <-removeDone; code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", code)
	}

	// The workspace the apply created still exists...
	if _, _, err := svc.Workspaces.LoadWithRevision("team-a"); err != nil {
		t.Fatalf("the applied workspace row is gone: %v", err)
	}
	// ...so its script has to as well.
	got, err := os.ReadFile(initScriptPathFor(configDir, "team-a"))
	if err != nil {
		t.Fatalf("the removal deleted a script written by an apply that had already committed and answered 200: %v", err)
	}
	if string(got) != "script B\n" {
		t.Errorf("on-disk init.sh = %q, want %q", got, "script B\n")
	}
}

// TestWorkspaceHandler_Remove_KeepsAScriptWrittenWhileTheHomeVolumeIsDeleted is
// the same guarantee one step later in the request.
//
// The HOME-volume deletion runs after RemoveWorkspace has returned, outside the
// mutex, and takes as long as the container engine takes. An apply landing in
// THAT window is a complete, successful operation against a workspace that
// exists again, and nothing left in the removal may touch it.
func TestWorkspaceHandler_Remove_KeepsAScriptWrittenWhileTheHomeVolumeIsDeleted(t *testing.T) {
	svc, configDir := newInitScriptTestService(t, "team-a")
	if _, err := svc.SetWorkspaceInitScript("team-a", []byte("script A\n"), WorkspaceInitScriptAbsentRevision, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	homes := &pauseInsideHomeDeletion{
		WorkspaceHomeStore: newStubHomeStore(map[string]int64{"team-a": 4096}),
		inside:             make(chan struct{}),
		resume:             make(chan struct{}),
	}
	h := &WorkspaceHandler{Service: svc, Homes: homes}

	removeDone := make(chan int, 1)
	go func() {
		removeDone <- doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil).Code
	}()

	select {
	case <-homes.inside:
	case <-time.After(10 * time.Second):
		t.Fatal("DELETE never reached the home-volume deletion")
	}

	// The row is gone and the removal is still in flight. An apply re-creating
	// the workspace here is a complete, successful operation — it must not
	// have its script deleted out from under it by the removal that is still
	// finishing.
	apply := envelopeApplyDoc("team-a", map[string]bool{"init_script": true},
		orchestrator.WorkspaceEnvelopeSpec{InitScript: "script B\n"})
	result, err := svc.ApplyWorkspace(apply, false)
	if err != nil {
		t.Fatalf("concurrent ApplyWorkspace: %v", err)
	}
	if result.InitScriptAction != WorkspaceInitScriptWritten {
		t.Fatalf("concurrent apply reported InitScriptAction = %q, want %q", result.InitScriptAction, WorkspaceInitScriptWritten)
	}

	close(homes.resume)
	if code := <-removeDone; code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", code)
	}

	// The workspace the apply created still exists...
	if _, _, err := svc.Workspaces.LoadWithRevision("team-a"); err != nil {
		t.Fatalf("the applied workspace row is gone: %v", err)
	}
	// ...so its script has to as well.
	got, err := os.ReadFile(initScriptPathFor(configDir, "team-a"))
	if err != nil {
		t.Fatalf("the removal deleted a script written by an apply that had already committed and answered 200: %v", err)
	}
	if string(got) != "script B\n" {
		t.Errorf("on-disk init.sh = %q, want %q", got, "script B\n")
	}
}

// ---------------------------------------------------------------------------
// mode repair on an unchanged write (PR9 codex round 3, Minor)
// ---------------------------------------------------------------------------

// stagePrePR9InitScript writes slug's init.sh the way the hand-managed,
// pre-PR9 workflow left it on the install this plan doc migrates: a
// group-writable directory (0775) and a group-writable, executable file
// (0755).
func stagePrePR9InitScript(t *testing.T, configDir, slug string, content []byte) string {
	t.Helper()
	path := initScriptPathFor(configDir, slug)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatalf("stage the directory: %v", err)
	}
	if err := os.Chmod(dir, 0o775); err != nil { // MkdirAll's mode is subject to umask
		t.Fatalf("stage the directory mode: %v", err)
	}
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("stage the file: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("stage the file mode: %v", err)
	}
	return path
}

func assertInitScriptModes(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the script: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("init.sh mode = %04o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat the directory: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("workspace config dir mode = %04o, want 0700 (a group-writable directory lets anyone in the group swap init.sh for their own)", got)
	}
}

// TestSetWorkspaceInitScript_RepairsTheModesEvenWhenTheContentIsUnchanged is
// the Minor.
//
// The service short-circuits identical content to "unchanged" and never
// reaches the store, so re-setting (or re-applying) the script an operator
// already has left the 0775 directory and the 0755 file exactly as the
// hand-managed workflow left them — the very state PR9 added the per-write
// chmod for. The store-level test that covered the chmod called the store
// directly and therefore never went through this short circuit.
//
// "Unchanged" is a statement about the CONTENT, and stays one: the action is
// still reported as unchanged, and the completion marker's hash is unaffected,
// so nothing re-runs init.
func TestSetWorkspaceInitScript_RepairsTheModesEvenWhenTheContentIsUnchanged(t *testing.T) {
	script := []byte("echo hi\n")

	t.Run("set", func(t *testing.T) {
		svc, configDir := newInitScriptTestService(t, "team-a")
		path := stagePrePR9InitScript(t, configDir, "team-a", script)

		res, err := svc.SetWorkspaceInitScript("team-a", script, workspaceInitScriptRevision(script, true), false)
		if err != nil {
			t.Fatalf("SetWorkspaceInitScript: %v", err)
		}
		if res.Action != WorkspaceInitScriptUnchanged {
			t.Errorf("action = %q, want %q", res.Action, WorkspaceInitScriptUnchanged)
		}
		assertInitScriptModes(t, path)
		if got, _ := os.ReadFile(path); string(got) != string(script) {
			t.Errorf("content = %q, want it untouched (%q)", got, script)
		}
	})

	t.Run("apply", func(t *testing.T) {
		svc, configDir := newInitScriptTestService(t, "team-a")
		path := stagePrePR9InitScript(t, configDir, "team-a", script)

		apply := envelopeApplyDoc("team-a", map[string]bool{"init_script": true},
			orchestrator.WorkspaceEnvelopeSpec{InitScript: string(script)})
		result, err := svc.ApplyWorkspace(apply, false)
		if err != nil {
			t.Fatalf("ApplyWorkspace: %v", err)
		}
		if result.InitScriptAction != WorkspaceInitScriptUnchanged {
			t.Errorf("InitScriptAction = %q, want %q", result.InitScriptAction, WorkspaceInitScriptUnchanged)
		}
		assertInitScriptModes(t, path)
	})

	// A dry run still writes nothing at all — including a chmod.
	t.Run("dry run repairs nothing", func(t *testing.T) {
		svc, configDir := newInitScriptTestService(t, "team-a")
		path := stagePrePR9InitScript(t, configDir, "team-a", script)

		apply := envelopeApplyDoc("team-a", map[string]bool{"init_script": true},
			orchestrator.WorkspaceEnvelopeSpec{InitScript: string(script)})
		if _, err := svc.ApplyWorkspace(apply, true); err != nil {
			t.Fatalf("ApplyWorkspace(dryRun): %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o755 {
			t.Errorf("a dry run changed the file mode to %04o", got)
		}
	})
}

// assertStatusError fails the test unless err is a *StatusError with the given
// code.
func assertStatusError(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a *StatusError{%d}, got nil", want)
	}
	serr, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected a *StatusError{%d}, got %T: %v", want, err, err)
	}
	if serr.Code != want {
		t.Fatalf("status = %d, want %d: %v", serr.Code, want, err)
	}
}

package orchestrator

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// PR9 of docs/plans/workspace-home-volume-persistence.md (論点 d), the
// envelope half: spec.init_script carries a workspace's init.sh through
// `boid workspace export` / `apply`, which is what makes an export a complete
// backup and what moves the existing host-side scripts into the daemon's
// volume.
//
// D1: the field is on the ENVELOPE spec, not on WorkspaceMeta. WorkspaceMeta
// is a DB row (workspaces table columns) and init.sh is deliberately a file
// (workspaceInitScriptPath's doc comment), so putting it there would mean a
// migration plus contradicting a standing decision. The consequence for this
// package is that MergeInto must NOT touch it — hydration is a separate path
// in internal/api, which owns the store.

// TestWorkspaceEnvelope_InitScriptPresenceSemantics pins the field's
// missing-vs-explicitly-empty distinction, the same one every other spec field
// carries (see WorkspaceEnvelopeSpec's doc comment on why none of them use
// `omitempty`).
//
// It is load-bearing for init_script specifically: "the key is absent" has to
// mean "leave the workspace's init.sh alone" and `init_script: ""` has to mean
// "this workspace has no init.sh — delete the one it has". Collapsing the two
// would make an export of a script-less workspace unable to clear a stale
// script on the target, which is exactly the failure the omitempty removal
// fixed for env/extra_repos.
func TestWorkspaceEnvelope_InitScriptPresenceSemantics(t *testing.T) {
	tests := []struct {
		name        string
		spec        string
		wantPresent bool
		wantValue   string
	}{
		{
			name:        "absent",
			spec:        "  env: {}\n",
			wantPresent: false,
		},
		{
			name:        "explicitly empty",
			spec:        "  init_script: \"\"\n",
			wantPresent: true,
			wantValue:   "",
		},
		{
			name:        "content",
			spec:        "  init_script: |\n    echo hi\n",
			wantPresent: true,
			wantValue:   "echo hi\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\nspec:\n" + tt.spec)
			docs, err := DecodeWorkspaceEnvelopeDocuments(data)
			if err != nil {
				t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
			}
			doc := docs[0]
			if got := doc.FieldsPresent["init_script"]; got != tt.wantPresent {
				t.Errorf("FieldsPresent[\"init_script\"] = %v, want %v", got, tt.wantPresent)
			}
			if got := doc.Envelope.Spec.InitScript; got != tt.wantValue {
				t.Errorf("Spec.InitScript = %q, want %q", got, tt.wantValue)
			}
		})
	}
}

// TestWorkspaceEnvelope_InitScriptIsAllowListed pins the other half of
// decodeWorkspaceEnvelopeSpec's contract: adding a field to the struct without
// adding it to workspaceEnvelopeSpecFields makes every document carrying it
// fail with "unknown field", so an export produced by this very binary would
// be rejected by `apply`.
func TestWorkspaceEnvelope_InitScriptIsAllowListed(t *testing.T) {
	if !workspaceEnvelopeSpecFields["init_script"] {
		t.Fatal("init_script is not in workspaceEnvelopeSpecFields; every exported document would be rejected as an unknown field")
	}
}

// TestMergeInto_LeavesInitScriptToTheSeparateHydrationPath pins D1's negative
// half: init_script is not a WorkspaceMeta field, so a merge must not invent
// one. The test is a compile-and-behaviour guard against someone "finishing
// the job" by adding an InitScript column later — WorkspaceMeta has no such
// field and MergeInto returns a *WorkspaceMeta, so the only way this could
// regress is by a schema change, which this test's existence flags as a
// decision rather than a detail.
func TestMergeInto_LeavesInitScriptToTheSeparateHydrationPath(t *testing.T) {
	doc := &WorkspaceEnvelopeApply{
		Envelope: &WorkspaceEnvelope{Spec: WorkspaceEnvelopeSpec{
			InitScript:   "echo hi\n",
			HostCommands: []string{"gh"},
		}},
		FieldsPresent: map[string]bool{"init_script": true, "host_commands": true},
	}
	merged := doc.MergeInto(&WorkspaceMeta{})
	if merged == nil {
		t.Fatal("MergeInto returned nil")
	}
	// The merge still does its own job.
	if !equalStrSlice(merged.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", merged.HostCommands)
	}
	// And nothing about the script leaked into the row that gets persisted.
	out, err := yaml.Marshal(merged)
	if err != nil {
		t.Fatalf("marshal merged meta: %v", err)
	}
	if strings.Contains(string(out), "echo hi") {
		t.Errorf("merged WorkspaceMeta = %q, want no trace of spec.init_script (it is a file, not a DB column)", out)
	}
}

// TestNewWorkspaceEnvelopeFromMeta_InitScriptSurvivesExport pins the export
// side of the presence contract: a workspace with NO init.sh exports
// `init_script: ""`, which on the way back in reads as "clear it" rather than
// "do not touch it".
func TestNewWorkspaceEnvelopeFromMeta_InitScriptSurvivesExport(t *testing.T) {
	envelope := NewWorkspaceEnvelopeFromMeta("team-empty", &WorkspaceMeta{}, nil, "")
	data, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `init_script: ""`) {
		t.Errorf("exported yaml = %q, want it to contain %q", string(data), `init_script: ""`)
	}
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !docs[0].FieldsPresent["init_script"] {
		t.Error("FieldsPresent[\"init_script\"] = false after exporting a script-less workspace; a backup could never clear a stale script")
	}
}

// TestNewWorkspaceEnvelopeFromMeta_InitScriptRoundTripsByteForByte is the
// fidelity guard. An init.sh is hashed into the completion marker
// (dispatcher's scriptSHA256Hex), so a single byte changed by the yaml round
// trip is not cosmetic: it re-runs init on every workspace restored from a
// backup, and — worse — it runs a script that is not the one the operator
// wrote.
//
// The cases are the ones a yaml emitter is most likely to normalise: a
// trailing-whitespace line and a missing final newline both disqualify the
// literal block style, forcing the emitter into a quoted style; tabs, CR, and
// non-ASCII are the classic escape-and-lose-it candidates.
func TestNewWorkspaceEnvelopeFromMeta_InitScriptRoundTripsByteForByte(t *testing.T) {
	scripts := map[string]string{
		"plain":                "#!/bin/sh\necho hi\n",
		"no trailing newline":  "echo hi",
		"trailing spaces":      "echo hi   \necho bye\n",
		"tabs":                 "if true; then\n\techo hi\nfi\n",
		"carriage returns":     "echo one\r\necho two\r\n",
		"non-ascii":            "echo 'こんにちは'\n",
		"blank trailing lines": "echo hi\n\n\n",
		"single quotes":        "echo '\"quoted\"'\n",
		"leading whitespace":   "   echo indented\n",
		// The leading-blank family (PR9 codex round 2, Major 3). A script whose
		// FIRST line is empty or starts with a tab is what decides the block
		// scalar's indentation, and it is the one thing gopkg.in/yaml.v3's
		// emitter gets wrong — see
		// TestWorkspaceEnvelope_InitScriptRoundTripsEveryBlankFirstLine.
		"leading tab":            "\techo indented\n",
		"leading blank line":     "\necho hi\n",
		"leading blank and tab":  "\n\techo hi\n",
		"tab-only first line":    "\t\necho hi\n",
		"heredoc-style leading":  "\t\t# tab-indented header\necho hi\n",
		"leading crlf blankline": "\r\necho hi\n",
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			envelope := NewWorkspaceEnvelopeFromMeta("team-a", &WorkspaceMeta{}, nil, script)
			data, err := yaml.Marshal(envelope)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			docs, err := DecodeWorkspaceEnvelopeDocuments(data)
			if err != nil {
				t.Fatalf("round trip decode: %v\ndocument:\n%s", err, data)
			}
			if got := docs[0].Envelope.Spec.InitScript; got != script {
				t.Errorf("init_script round trip = %q, want %q\ndocument:\n%s", got, script, data)
			}
		})
	}
}

// TestWorkspaceEnvelope_InitScriptRoundTripsEveryBlankFirstLine is the
// exhaustive version of the leading-blank cases above, and the regression net
// for PR9 codex round 2's Major 3 ("an accepted script must round-trip").
//
// # What was broken
//
// gopkg.in/yaml.v3's emitter picks the literal block style for any multi-line
// string, and a block scalar's indentation is inferred by the READER from its
// first line. When that first line is empty or starts with a TAB the inference
// cannot work, and the emitter has to say the indentation out loud (`|4`). It
// does so for a first line starting with a SPACE and not for one starting with
// a tab, which produced two distinct failures, both silent at export time:
//
//   - first line starts with a tab: the emitted document is not valid YAML at
//     all. `boid workspace apply` of that export fails with "found a tab
//     character where an indentation space is expected" — the backup is
//     unrestorable, and nothing said so when it was taken;
//   - first line is empty: the leading empty line(s) are DROPPED. The document
//     parses, the apply succeeds, and the workspace comes back with a script
//     that is not the one that was exported — which also re-runs init, since
//     the completion marker hashes the bytes.
//
// Neither is exotic: a tab-indented first line is what a script pasted out of a
// Makefile or a heredoc looks like, and a leading blank line is what an editor
// leaves behind.
//
// # Why it is exhaustive rather than a list of examples
//
// The bug is entirely decided by the leading run of blanks, so the input space
// that matters is small enough to enumerate: every string up to 4 characters
// over {ordinary character, space, tab, newline}. A hand-picked list would have
// missed the leading-empty-line half — the tab was found first and reads like
// the whole story.
func TestWorkspaceEnvelope_InitScriptRoundTripsEveryBlankFirstLine(t *testing.T) {
	alphabet := []byte{'a', ' ', '\t', '\n'}
	var check func(prefix []byte, depth int)
	check = func(prefix []byte, depth int) {
		if depth == 0 {
			script := string(prefix)
			envelope := NewWorkspaceEnvelopeFromMeta("team-a", &WorkspaceMeta{}, nil, script)
			data, err := yaml.Marshal(envelope)
			if err != nil {
				t.Errorf("script %q: marshal: %v", script, err)
				return
			}
			docs, err := DecodeWorkspaceEnvelopeDocuments(data)
			if err != nil {
				t.Errorf("script %q: the exported document cannot be applied: %v\ndocument:\n%s", script, err, data)
				return
			}
			if got := docs[0].Envelope.Spec.InitScript; got != script {
				t.Errorf("script %q: round trip = %q\ndocument:\n%s", script, got, data)
			}
			return
		}
		for _, c := range alphabet {
			check(append(prefix, c), depth-1)
		}
	}
	for depth := 1; depth <= 4; depth++ {
		check(nil, depth)
	}
}

// TestWorkspaceEnvelope_InitScriptRoundTripsNonUTF8 keeps the fix above from
// costing the one case yaml.v3 already handles correctly.
//
// A script that is not valid UTF-8 is emitted as `!!binary` (base64), which has
// no first-line problem and round-trips exactly. Forcing a quoted style on it
// instead would be a regression, so the encoder's own choice has to be left
// alone there — and this is what says so.
func TestWorkspaceEnvelope_InitScriptRoundTripsNonUTF8(t *testing.T) {
	scripts := map[string]string{
		"invalid utf-8":               "echo \xff\xfe\n",
		"invalid utf-8 leading tab":   "\techo \xff\n",
		"invalid utf-8 leading blank": "\necho \xff\n",
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			envelope := NewWorkspaceEnvelopeFromMeta("team-a", &WorkspaceMeta{}, nil, script)
			data, err := yaml.Marshal(envelope)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			docs, err := DecodeWorkspaceEnvelopeDocuments(data)
			if err != nil {
				t.Fatalf("round trip decode: %v\ndocument:\n%s", err, data)
			}
			if got := docs[0].Envelope.Spec.InitScript; got != script {
				t.Errorf("init_script round trip = %q, want %q\ndocument:\n%s", got, script, data)
			}
		})
	}
}

// TestSplitWorkspaceEnvelopeDocuments_KeepsEachDocumentApplicable pins the
// second emitter in the apply path.
//
// `boid workspace apply` does not POST the file's bytes: it decodes each
// document and RE-MARSHALS it (SplitWorkspaceEnvelopeDocuments, so that field
// presence survives), which means the same style choice is made a second time,
// on a node tree this time rather than on the struct. A document that parsed
// fine on the way in must still be applicable on the way out — otherwise the
// CLI turns a valid file into a 400 from its own daemon.
func TestSplitWorkspaceEnvelopeDocuments_KeepsEachDocumentApplicable(t *testing.T) {
	// Written the way an operator would have to write it by hand for a
	// tab-indented first line: an explicit block indentation indicator.
	const doc = "apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\nspec:\n" +
		"  init_script: |2\n    \techo indented\n    echo hi\n"
	want := "\techo indented\necho hi\n"

	docs, err := DecodeWorkspaceEnvelopeDocuments([]byte(doc))
	if err != nil {
		t.Fatalf("decode the hand-written document: %v", err)
	}
	if got := docs[0].Envelope.Spec.InitScript; got != want {
		t.Fatalf("the hand-written document itself does not say what this test assumes: %q", got)
	}

	raws, err := SplitWorkspaceEnvelopeDocuments([]byte(doc))
	if err != nil {
		t.Fatalf("SplitWorkspaceEnvelopeDocuments: %v", err)
	}
	redecoded, err := DecodeWorkspaceEnvelopeDocuments(raws[0])
	if err != nil {
		t.Fatalf("the re-marshaled document is not applicable: %v\ndocument:\n%s", err, raws[0])
	}
	if got := redecoded[0].Envelope.Spec.InitScript; got != want {
		t.Errorf("init_script after the split re-marshal = %q, want %q\ndocument:\n%s", got, want, raws[0])
	}
}

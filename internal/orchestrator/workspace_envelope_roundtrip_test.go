package orchestrator

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// PR9 codex round 3, Major 1: the round-trippable-output fix generalized.
//
// Round 2's Major 3 established the guarantee "a successful export can always
// be applied" (docs/plans/workspace-home-volume-persistence.md D13-b) and
// implemented it for spec.init_script alone. The defect it works around is a
// property of gopkg.in/yaml.v3's EMITTER, not of that one field, so it holds
// for every string the envelope carries — an env value, an env KEY, a
// host_commands entry, an extra_repos entry, container_image, a project name or
// url. These tests are the field-independent version of round 2's exhaustive
// leading-blank sweep.

// roundTripAlphabet is the four character classes that decide yaml.v3's block
// scalar behaviour: an ordinary character, a space (the case the emitter
// already handles with an explicit `|4` indicator), a TAB (which can never be
// YAML indentation, so an unindicated block scalar is invalid YAML) and a
// newline (an empty first line, which the reader cannot infer indentation from
// and which is silently dropped).
var roundTripAlphabet = []byte{'a', ' ', '\t', '\n'}

// forEachRoundTripProbe calls fn with every string up to maxLen characters over
// roundTripAlphabet — the same exhaustive enumeration
// TestWorkspaceEnvelope_InitScriptRoundTripsEveryBlankFirstLine runs for
// init_script, factored out so every other string-bearing slot gets exactly the
// same coverage rather than a hand-picked sample of it.
func forEachRoundTripProbe(maxLen int, fn func(probe string)) {
	var walk func(prefix []byte, depth int)
	walk = func(prefix []byte, depth int) {
		if depth == 0 {
			fn(string(prefix))
			return
		}
		for _, c := range roundTripAlphabet {
			walk(append(prefix, c), depth-1)
		}
	}
	for depth := 1; depth <= maxLen; depth++ {
		walk(nil, depth)
	}
}

// envelopeWithProbeEverywhere builds an envelope carrying probe in every
// string-typed slot of the spec at once.
//
// All of them at once rather than one per case: it is both the cheaper sweep
// and the stronger one, since a fix that rewrites offending scalars has to keep
// several of them apart within a single document.
//
// The project name is probe+"p" because a whitespace-only name is refused
// outright by decodeWorkspaceEnvelopeSpec (and by export before that), so it is
// not a string the envelope has to be able to carry. The suffix leaves the
// FIRST character — the only one the emitter's block-scalar choice depends on —
// exactly as probe has it.
func envelopeWithProbeEverywhere(probe string) *WorkspaceEnvelope {
	meta := &WorkspaceMeta{
		HostCommands:   []string{probe},
		Env:            map[string]string{"PROBE": probe, probe: "probe-key"},
		AllowedDomains: []string{probe},
		ExtraRepos:     []string{probe},
		ContainerImage: probe,
		// TaskBehaviors carries probe inside a Traits value (the map KEY is
		// fixed as "probe", not the probe content itself — a weird
		// task_behaviors NAME is not this test's concern, and keeping it
		// fixed sidesteps normalizeWorkspaceDefaultTaskBehaviors's alias
		// handling entirely). Also gives this field a non-nil value on the
		// "want" side so it does not spuriously mismatch the decoded "got"
		// side's non-nil-but-empty map for the probes that leave every OTHER
		// field's content trivial — yaml.v3 decodes an empty `{}` mapping
		// into a non-nil zero-length Go map (verified empirically), so an
		// unset (nil) TaskBehaviors here would never DeepEqual what comes
		// back out of a real round trip regardless of probe content.
		TaskBehaviors:       map[string]TaskBehavior{"probe": {Traits: []string{probe}}},
		BaseBranch:          probe,
		ForkPoint:           probe,
		DefaultTaskBehavior: probe,
	}
	projects := []WorkspaceEnvelopeProject{{Name: probe + "p", URL: probe}}
	return NewWorkspaceEnvelopeFromMeta("team-a", meta, projects, probe)
}

// TestWorkspaceEnvelope_EveryStringSlotRoundTripsThroughExport is Major 1.
//
// An export whose env value happens to begin with a tab emits a document that
// is not valid YAML, and one that begins with a newline emits a document whose
// leading blank line is silently dropped — the same two failures round 2 found
// for init_script, on a field nothing protected. Both are silent at export
// time and only visible at restore time, which is the point of the guarantee.
func TestWorkspaceEnvelope_EveryStringSlotRoundTripsThroughExport(t *testing.T) {
	forEachRoundTripProbe(4, func(probe string) {
		envelope := envelopeWithProbeEverywhere(probe)
		data, err := yaml.Marshal(envelope)
		if err != nil {
			t.Errorf("probe %q: marshal: %v", probe, err)
			return
		}
		docs, err := DecodeWorkspaceEnvelopeDocuments(data)
		if err != nil {
			t.Errorf("probe %q: the exported document cannot be applied: %v\ndocument:\n%s", probe, err, data)
			return
		}
		if got, want := docs[0].Envelope.Spec, envelope.Spec; !reflect.DeepEqual(got, want) {
			t.Errorf("probe %q: spec round trip mismatch\n got: %#v\nwant: %#v\ndocument:\n%s", probe, got, want, data)
		}
	})
}

// TestSplitWorkspaceEnvelopeDocuments_KeepsEveryStringSlotApplicable is the
// same sweep for the apply path's second emitter: `boid workspace apply`
// re-marshals each decoded document before POSTing it, so a document that
// parsed on the way in has to still parse on the way out.
func TestSplitWorkspaceEnvelopeDocuments_KeepsEveryStringSlotApplicable(t *testing.T) {
	forEachRoundTripProbe(4, func(probe string) {
		envelope := envelopeWithProbeEverywhere(probe)
		data, err := yaml.Marshal(envelope)
		if err != nil {
			t.Errorf("probe %q: marshal: %v", probe, err)
			return
		}
		raws, err := SplitWorkspaceEnvelopeDocuments(data)
		if err != nil {
			t.Errorf("probe %q: SplitWorkspaceEnvelopeDocuments: %v\ndocument:\n%s", probe, err, data)
			return
		}
		docs, err := DecodeWorkspaceEnvelopeDocuments(raws[0])
		if err != nil {
			t.Errorf("probe %q: the re-marshaled document is not applicable: %v\ndocument:\n%s", probe, err, raws[0])
			return
		}
		if got, want := docs[0].Envelope.Spec, envelope.Spec; !reflect.DeepEqual(got, want) {
			t.Errorf("probe %q: spec mismatch after the split re-marshal\n got: %#v\nwant: %#v\ndocument:\n%s", probe, got, want, raws[0])
		}
	})
}

// TestWorkspaceEnvelope_OrdinaryValuesAreEmittedUnchanged is the other half of
// Major 1: the generalized fix must not cost the readability of every export
// that never had the problem.
//
// A golden document rather than a property, because "readable" is exactly what
// a property cannot state. Every style yaml.v3 picks for an ordinary workspace
// is pinned here — plain scalars, the single-quoted `it's`, the quoted "42"
// that would otherwise read as an int, a multi-line env value as a literal
// block, and the init script as a literal block. This is byte-for-byte what the
// pre-fix emitter produced (verified by diffing the two emitters over a dozen
// representative envelopes); exactly two shapes have their output changed by the
// fix, and neither is here — a value whose first line begins with a space, which
// becomes double-quoted because the rule is a property of the string and not of
// where it sits (needsRoundTripQuoting), and a value that begins with the marker
// rune, which is double-quoted as a consequence of the marker being injective
// (TestWorkspaceEnvelope_AMarkerPrefixedValueIsQuotedNotPlain).
func TestWorkspaceEnvelope_OrdinaryValuesAreEmittedUnchanged(t *testing.T) {
	envelope := NewWorkspaceEnvelopeFromMeta("team-a", &WorkspaceMeta{
		HostCommands: []string{"gh", "docker"},
		Env: map[string]string{
			"GOPATH": "/home/boid/go",
			"MULTI":  "one\ntwo\n",
			"EMPTY":  "",
			"SPACEY": "a b",
			"QUOTED": "it's",
			"NUM":    "42",
			"TRAIL":  "x   \ny\n",
		},
		AllowedDomains: []string{"proxy.golang.org", ".cosmos.azure.com"},
		ExtraRepos:     []string{"https://github.com/novshi-tech/boid-kits.git"},
		ContainerImage: "ghcr.io/novshi-tech/boid:latest",
		Capabilities:   Capabilities{Docker: &DockerCapability{}},
	}, []WorkspaceEnvelopeProject{
		{Name: "boid", URL: "https://github.com/novshi-tech/boid.git"},
		{Name: "no-url"},
	}, "#!/bin/sh\nset -eu\nexport PATH=\"$HOME/.local/bin:$PATH\"\nif true; then\n\techo hi\nfi\n")

	const want = `apiVersion: boid.dev/v1
kind: Workspace
metadata:
    name: team-a
spec:
    host_commands:
        - gh
        - docker
    env:
        EMPTY: ""
        GOPATH: /home/boid/go
        MULTI: |
            one
            two
        NUM: "42"
        QUOTED: it's
        SPACEY: a b
        TRAIL: "x   \ny\n"
    allowed_domains:
        - proxy.golang.org
        - .cosmos.azure.com
    extra_repos:
        - https://github.com/novshi-tech/boid-kits.git
    container_image: ghcr.io/novshi-tech/boid:latest
    capabilities:
        docker: {}
    projects:
        - name: boid
          url: https://github.com/novshi-tech/boid.git
        - name: no-url
    task_behaviors: {}
    base_branch: ""
    fork_point: ""
    default_task_behavior: ""
    init_script: |
        #!/bin/sh
        set -eu
        export PATH="$HOME/.local/bin:$PATH"
        if true; then
        	echo hi
        fi
`
	got, err := MarshalWorkspaceEnvelope(envelope)
	if err != nil {
		t.Fatalf("MarshalWorkspaceEnvelope: %v", err)
	}
	if string(got) != want {
		t.Errorf("an ordinary workspace no longer exports the way it did:\n got:\n%s\nwant:\n%s", got, want)
	}
	// And what yaml.Marshal produces has to be the same thing, since that is
	// what every other caller (and every test above) reaches for.
	direct, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if string(direct) != string(got) {
		t.Errorf("yaml.Marshal and MarshalWorkspaceEnvelope disagree:\n%s\n---\n%s", direct, got)
	}
}

// TestMarshalWorkspaceEnvelope_VerifiesItsOwnOutput runs the same exhaustive
// sweep through the function every EXPORT actually goes through, which
// yaml.Marshal alone does not exercise: it re-parses what it emitted and
// refuses to return a document that does not read back the same.
//
// A success here is therefore two claims at once — the document is applicable,
// and the check that says so did not have to fire.
func TestMarshalWorkspaceEnvelope_VerifiesItsOwnOutput(t *testing.T) {
	forEachRoundTripProbe(4, func(probe string) {
		envelope := envelopeWithProbeEverywhere(probe)
		data, err := MarshalWorkspaceEnvelope(envelope)
		if err != nil {
			t.Errorf("probe %q: MarshalWorkspaceEnvelope: %v", probe, err)
			return
		}
		docs, err := DecodeWorkspaceEnvelopeDocuments(data)
		if err != nil {
			t.Errorf("probe %q: %v\ndocument:\n%s", probe, err, data)
			return
		}
		if got, want := docs[0].Envelope.Spec, envelope.Spec; !reflect.DeepEqual(got, want) {
			t.Errorf("probe %q: spec round trip mismatch\ndocument:\n%s", probe, data)
		}
	})
}

// TestMarshalWorkspaceEnvelope_RefusesNonUTF8ThatWouldLeakTheMarker is the
// exception markRoundTripStrings carves out, asserted rather than assumed.
//
// A string that is not valid UTF-8 is emitted as `!!binary`, whose emitted
// value is base64 — so a marker prefixed onto it would have no marker left in
// the document to strip back off, and would silently become part of the
// restored bytes. The carve-out means such a string is never marked, including
// when it happens to start with the marker rune itself.
func TestMarshalWorkspaceEnvelope_RefusesNonUTF8ThatWouldLeakTheMarker(t *testing.T) {
	for name, script := range map[string]string{
		"invalid utf-8":                    "echo \xff\xfe\n",
		"invalid utf-8 after the marker":   roundTripMarker + "echo \xff\n",
		"invalid utf-8 with a leading tab": "\techo \xff\n",
	} {
		t.Run(name, func(t *testing.T) {
			envelope := NewWorkspaceEnvelopeFromMeta("team-a", &WorkspaceMeta{}, nil, script)
			data, err := MarshalWorkspaceEnvelope(envelope)
			if err != nil {
				t.Fatalf("MarshalWorkspaceEnvelope: %v", err)
			}
			if strings.Contains(string(data), roundTripMarker) {
				t.Errorf("the internal marker leaked into the document:\n%s", data)
			}
			docs, err := DecodeWorkspaceEnvelopeDocuments(data)
			if err != nil {
				t.Fatalf("round trip: %v\ndocument:\n%s", err, data)
			}
			if got := docs[0].Envelope.Spec.InitScript; got != script {
				t.Errorf("init_script = %q, want %q\ndocument:\n%s", got, script, data)
			}
		})
	}
}

// TestMarshalWorkspaceEnvelope_MarkerBearingValuesRoundTrip pins the other end
// of the marker's injectivity: a value that legitimately CONTAINS the marker
// rune must come back with exactly as many of them as it went in with.
func TestMarshalWorkspaceEnvelope_MarkerBearingValuesRoundTrip(t *testing.T) {
	values := []string{
		roundTripMarker,
		roundTripMarker + "plain",
		roundTripMarker + "\tleading tab\nsecond\n",
		roundTripMarker + roundTripMarker + "\nleading blank\n",
		"mid" + roundTripMarker + "marker\n",
	}
	for _, v := range values {
		envelope := NewWorkspaceEnvelopeFromMeta("team-a",
			&WorkspaceMeta{Env: map[string]string{"PROBE": v, v: "probe-key"}}, nil, v)
		data, err := MarshalWorkspaceEnvelope(envelope)
		if err != nil {
			t.Errorf("value %q: MarshalWorkspaceEnvelope: %v", v, err)
			continue
		}
		docs, err := DecodeWorkspaceEnvelopeDocuments(data)
		if err != nil {
			t.Errorf("value %q: %v\ndocument:\n%s", v, err, data)
			continue
		}
		spec := docs[0].Envelope.Spec
		if spec.InitScript != v || spec.Env["PROBE"] != v || spec.Env[v] != "probe-key" {
			t.Errorf("value %q did not round trip: init_script=%q env=%#v\ndocument:\n%s", v, spec.InitScript, spec.Env, data)
		}
	}
}

// TestWorkspaceEnvelope_AMarkerPrefixedValueIsQuotedNotPlain names the SECOND
// exception to "every other value is emitted byte-for-byte as it was before this
// fix" (PR9 codex round 4 final, Minor 1).
//
// The documented exception is the leading-blank family. But
// roundTripStringNeedsMarking also marks a string that merely STARTS WITH the
// marker rune — it has to, or the marker would not be injective and
// unmarkRoundTripScalars could not tell a marked string from one that legitimately
// begins with U+E000 (TestMarshalWorkspaceEnvelope_MarkerBearingValuesRoundTrip
// is what that buys). Marking pins the scalar to the double-quoted style, so a
// single-line `plain` — which needs no fix at all, and which the pre-fix
// emitter wrote as a plain scalar — now comes out quoted.
//
// Nothing about the restored VALUE changes, which is why the exception is worth
// only a line in the plan doc rather than a redesign. This test is here so that
// line is checkable, and so a future reading of "everything else is unchanged"
// as a property to enforce fails here instead of breaking injectivity.
func TestWorkspaceEnvelope_AMarkerPrefixedValueIsQuotedNotPlain(t *testing.T) {
	// `type plain` sheds MarshalYAML, so this is the pre-fix emitter.
	type plainEnvelope WorkspaceEnvelope
	emitBoth := func(t *testing.T, value string) (fixed, prefix string) {
		t.Helper()
		envelope := NewWorkspaceEnvelopeFromMeta("team-a", &WorkspaceMeta{ContainerImage: value}, nil, "")
		got, err := MarshalWorkspaceEnvelope(envelope)
		if err != nil {
			t.Fatalf("MarshalWorkspaceEnvelope(%q): %v", value, err)
		}
		before, err := yaml.Marshal(plainEnvelope(*envelope))
		if err != nil {
			t.Fatalf("pre-fix marshal(%q): %v", value, err)
		}
		return string(got), string(before)
	}

	// The control: an ordinary single-line value is still byte-identical.
	fixed, before := emitBoth(t, "ghcr.io/novshi-tech/boid:latest")
	if fixed != before {
		t.Errorf("an ordinary value's emitted bytes changed:\n got:\n%s\nwant:\n%s", fixed, before)
	}

	// The exception.
	const marked = roundTripMarker + "plain"
	fixed, before = emitBoth(t, marked)
	if fixed == before {
		t.Fatalf("expected a marker-prefixed value to be emitted differently from the pre-fix emitter:\n%s", fixed)
	}
	if !strings.Contains(fixed, "container_image: \""+marked+"\"") {
		t.Errorf("marker-prefixed value not emitted double-quoted:\n%s", fixed)
	}
	if !strings.Contains(before, "container_image: "+marked+"\n") {
		t.Errorf("pre-fix emitter is assumed to write this as a plain scalar; it did not:\n%s", before)
	}

	// ...and it is only a style difference: the value still reads back exactly.
	docs, err := DecodeWorkspaceEnvelopeDocuments([]byte(fixed))
	if err != nil {
		t.Fatalf("round trip: %v\ndocument:\n%s", err, fixed)
	}
	if got := docs[0].Envelope.Spec.ContainerImage; got != marked {
		t.Errorf("container_image = %q, want %q", got, marked)
	}
}

// TestDiffYAMLValues_DetectsWhatTheVerificationExistsToCatch pins the detector
// MarshalWorkspaceEnvelope's guarantee rests on. Without it, "the emitted
// document was verified" is a claim about a function nothing tests.
func TestDiffYAMLValues_DetectsWhatTheVerificationExistsToCatch(t *testing.T) {
	parse := func(t *testing.T, doc string) *yaml.Node {
		t.Helper()
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatalf("parse %q: %v", doc, err)
		}
		return n.Content[0]
	}
	tests := []struct {
		name      string
		a, b      string
		wantMatch bool
	}{
		{"identical", "spec:\n  env:\n    A: one\n", "spec:\n  env:\n    A: one\n", true},
		{"style differs only", "spec:\n  env:\n    A: one\n", "spec:\n  env:\n    A: \"one\"\n", true},
		{"scalar value differs", "spec:\n  env:\n    A: one\n", "spec:\n  env:\n    A: two\n", false},
		{"a leading blank line was dropped", "a: \"\\nhi\\n\"\n", "a: |\n  hi\n", false},
		{"key differs", "spec:\n  env:\n    A: one\n", "spec:\n  env:\n    B: one\n", false},
		{"entry missing", "spec:\n  a: [1, 2]\n", "spec:\n  a: [1]\n", false},
		{"kind differs", "spec:\n  a: x\n", "spec:\n  a: [x]\n", false},
		{"tag differs", "a: \"42\"\n", "a: 42\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff := diffYAMLValues("", parse(t, tt.a), parse(t, tt.b))
			if tt.wantMatch && diff != "" {
				t.Errorf("diffYAMLValues reported %q for two documents that carry the same values", diff)
			}
			if !tt.wantMatch && diff == "" {
				t.Error("diffYAMLValues reported no difference between documents that do not carry the same values")
			}
		})
	}
}

// TestWorkspaceEnvelope_NoNestedTypeOverridesTheEncoder pins the one
// assumption markRoundTripStrings rests on that reflection cannot enforce by
// itself.
//
// Marking rewrites the VALUE and lets yaml.v3 encode it. That is only complete
// while yaml.v3 actually walks the fields it rewrote: a nested type with its
// own MarshalYAML would be handed to the encoder as whatever IT returns, and a
// string reconstructed inside that method never passed through the marking at
// all. Nothing about adding such a method looks wrong, and the failure it
// produces is a silently unrestorable backup, so the constraint is stated here
// rather than left to be rediscovered.
//
// WorkspaceEnvelope itself is the exception and the reason: its MarshalYAML is
// the entry point that does the marking.
func TestWorkspaceEnvelope_NoNestedTypeOverridesTheEncoder(t *testing.T) {
	marshaler := reflect.TypeOf((*yaml.Marshaler)(nil)).Elem()
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		if path != "" && (typ.Implements(marshaler) || reflect.PointerTo(typ).Implements(marshaler)) {
			t.Errorf("%s (type %s) defines MarshalYAML: the encoder would take that method's output instead of the fields "+
				"markRoundTripStrings rewrote, so a string inside it could still be emitted in a form `workspace apply` cannot read back. "+
				"Either drop the method or make it go through encodeRoundTrippable.", path, typ)
		}
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				if !f.IsExported() {
					continue
				}
				walk(path+"."+f.Name, f.Type)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path+"[]", typ.Elem())
		case reflect.Map:
			walk(path+"<key>", typ.Key())
			walk(path+"[]", typ.Elem())
		}
	}
	walk("", reflect.TypeOf(WorkspaceEnvelope{}))
}

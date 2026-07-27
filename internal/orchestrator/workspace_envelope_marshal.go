package orchestrator

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// This file owns ONE guarantee for the workspace export/apply document
// (docs/plans/workspace-home-volume-persistence.md D13-b): a document `boid
// workspace export` emits can always be read back by `boid workspace apply`,
// and reads back as the same values.
//
// # The defect it works around
//
// gopkg.in/yaml.v3 emits any multi-line string as a literal block scalar, and a
// block scalar's indentation is inferred by the READER from its first line.
// That inference is impossible when the first line is empty or begins with a
// TAB (a tab can never be YAML indentation), so the emitter has to state the
// indentation explicitly — `|4`. v3.0.1 does not state it for a first line
// beginning with a tab or a newline, and states it WRONGLY (relative to the
// wrong node) for a first line beginning with a space once the scalar sits
// inside a block sequence entry or an explicit mapping key. The three failures
// are all silent at export time:
//
//   - first line starts with a TAB: the emitted document is not valid YAML.
//     Applying that export dies with "found a tab character where an
//     indentation space is expected" — an unrestorable backup;
//   - first line starts with a SPACE, in a sequence entry or an explicit key:
//     the emitted `|4` is not the indentation actually used, and the document
//     fails to parse ("did not find expected '-' indicator");
//   - first line is EMPTY: the leading empty line(s) are dropped. The document
//     parses and the apply succeeds, with a value that is not the exported one.
//
// # Why this is not a property of any one field
//
// PR9's first pass fixed it for spec.init_script alone, by special-casing that
// key on WorkspaceEnvelopeSpec. The defect belongs to the EMITTER, not to the
// schema, so it holds for every string the document carries — an env value, an
// env KEY, a host_commands / allowed_domains / extra_repos entry,
// container_image, a project name or url — and a per-field fix silently fails
// to cover the next field somebody adds.
//
// Nothing here enumerates fields. markRoundTripStrings walks the VALUE by
// reflection and unmarkRoundTripScalars walks the resulting NODE TREE, so a new
// spec field (of any string-bearing shape) is covered the moment it exists.
// MarshalWorkspaceEnvelope then re-parses its own output and refuses to return
// a document that does not compare equal — which makes the guarantee enforced
// rather than argued, including for a case this file's rule does not predict.
//
// # Why a quoted style rather than a corrected block scalar
//
// A *yaml.Node cannot request a block indentation indicator: Style selects
// among literal/folded/quoted and the indicator is chosen inside the emitter. A
// double-quoted scalar has no first-line rule at all, so it is the one style
// this package can ask for and be sure of. It is asked for ONLY for the
// leading-blank family — every other string keeps whatever style the encoder
// picked, so ordinary metadata and ordinary shell scripts are emitted exactly
// as they were before this file existed.
//
// Invalid UTF-8 is left to the encoder deliberately: it already picks
// `!!binary`, which is base64 and therefore has no first line to get wrong.
// Forcing a quoted style there would take a case that works and break it — see
// TestWorkspaceEnvelope_InitScriptRoundTripsNonUTF8.

// MarshalYAML makes every `yaml.Marshal` of an envelope round-trippable,
// wherever it is called from, rather than leaving the property to whichever
// call site remembered to ask for it.
//
// It returns a *yaml.Node: the encoder's own style choice is what has to be
// overridden, and a node is the only representation that can carry a style.
func (e WorkspaceEnvelope) MarshalYAML() (any, error) {
	return e.marshalNode()
}

// marshalNode is MarshalYAML's typed half, split out so
// MarshalWorkspaceEnvelope can compare the node it emitted against the document
// that came back.
func (e WorkspaceEnvelope) marshalNode() (*yaml.Node, error) {
	// `type plain` sheds MarshalYAML, so the encode below marshals the
	// envelope's fields exactly as the struct tags describe them — no second
	// copy of the field list to drift from the first, and no recursion.
	type plain WorkspaceEnvelope
	node, err := encodeRoundTrippable(plain(e))
	if err != nil {
		return nil, fmt.Errorf("encode workspace envelope: %w", err)
	}
	return node, nil
}

// MarshalWorkspaceEnvelope renders one envelope as the bytes of a single yaml
// document, and verifies that those bytes parse back into the same values.
//
// The verification is what turns D13-b from arithmetic into a property. Every
// caller that WRITES an export goes through here, so a string this file's rule
// does not anticipate — a yaml.v3 upgrade changing a style choice, a spec field
// of some shape nobody considered — fails the export loudly, naming the key,
// instead of producing a file that is discovered to be unrestorable at restore
// time. It cannot produce a false alarm on a document that is in fact fine: the
// comparison is against the emitter's own node tree, value for value.
//
// The size half of "applicable" lives with the caller
// (api.checkWorkspaceEnvelopeIsApplicable): it is a property of the daemon's
// request cap, which this package knows nothing about.
func MarshalWorkspaceEnvelope(e *WorkspaceEnvelope) ([]byte, error) {
	if e == nil {
		return nil, errors.New("marshal workspace envelope: nil envelope")
	}
	node, err := e.marshalNode()
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("marshal workspace envelope: %w", err)
	}
	var reparsed yaml.Node
	if err := yaml.Unmarshal(data, &reparsed); err != nil {
		return nil, fmt.Errorf(
			"marshal workspace envelope: the emitted document does not parse, so `workspace apply` could not read it back: %w", err)
	}
	root := &reparsed
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if diff := diffYAMLValues("", node, root); diff != "" {
		return nil, fmt.Errorf(
			"marshal workspace envelope: the emitted document does not read back as the workspace it describes (%s), "+
				"so applying it would restore something else", diff)
	}
	return data, nil
}

// roundTripMarker is the prefix markRoundTripStrings puts on a string the
// encoder cannot reproduce, and unmarkRoundTripScalars takes back off.
//
// # Why a marker at all
//
// (*yaml.Node).Encode marshals to text and re-parses it, so handing it one of
// these strings would hit the very defect this file exists for: it would fail
// to parse its own output, or parse it into a different value. The strings have
// to be made encodable BEFORE the encode and restored on the node afterwards.
//
// # Why a prefix rather than a table of placeholders
//
// A prefix is injective by construction, which a generated placeholder is only
// as long as nothing in the document happens to equal it. Prefixing is applied
// to any string that either needs the fix OR already starts with the marker, so
// stripping exactly one marker on the way back is exact for both.
//
// U+E000 is a private-use character: it is `printable` by the YAML spec (so
// yaml.v3 emits it literally rather than escaping it, and never reaches for
// !!binary because of it) and it is not a space, a tab or a newline — which is
// the whole requirement, since only the FIRST character decides the emitter's
// block-scalar behaviour.
const roundTripMarker = "\uE000"

// encodeRoundTrippable encodes v into a node tree whose every scalar can be
// emitted and read back unchanged.
func encodeRoundTrippable(v any) (*yaml.Node, error) {
	marked := markRoundTripStrings(reflect.ValueOf(v))
	var node yaml.Node
	if err := node.Encode(marked.Interface()); err != nil {
		return nil, err
	}
	unmarkRoundTripScalars(&node)
	return &node, nil
}

// markRoundTripStrings returns a copy of v with roundTripMarker prefixed onto
// every string that needs it, at any depth.
//
// Reflection rather than a hand-written traversal of WorkspaceEnvelope: the
// point of this fix is that it cannot miss a field, and a traversal that names
// its fields misses the next one added. Struct fields, slice and array
// elements, map VALUES and map KEYS are all covered, so is anything reachable
// through a pointer or an interface.
//
// Values whose kind carries no strings (numbers, bools, funcs, channels) are
// returned as-is rather than copied.
func markRoundTripStrings(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if !roundTripStringNeedsMarking(s) {
			return v
		}
		marked := reflect.New(v.Type()).Elem()
		marked.SetString(roundTripMarker + s)
		return marked

	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(markRoundTripStrings(v.Elem()))
		return out

	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(markRoundTripStrings(v.Elem()))
		return out

	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v) // unexported fields come along; yaml.v3 ignores them anyway.
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			out.Field(i).Set(markRoundTripStrings(v.Field(i)))
		}
		return out

	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(markRoundTripStrings(v.Index(i)))
		}
		return out

	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(markRoundTripStrings(v.Index(i)))
		}
		return out

	case reflect.Map:
		if v.IsNil() {
			return v
		}
		// Marking is injective, so two distinct keys cannot collide into one.
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(markRoundTripStrings(iter.Key()), markRoundTripStrings(iter.Value()))
		}
		return out

	default:
		return v
	}
}

// roundTripStringNeedsMarking reports whether s must be carried past the
// encoder under roundTripMarker.
//
// A string that is not valid UTF-8 is never marked, even when it starts with
// the marker: the encoder renders it as `!!binary`, whose emitted value is
// base64 and therefore carries no marker for unmarkRoundTripScalars to strip —
// marking it would leak the marker into the document.
func roundTripStringNeedsMarking(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	return needsRoundTripQuoting(s) || strings.HasPrefix(s, roundTripMarker)
}

// needsRoundTripQuoting reports whether s is in the family yaml.v3's block
// scalar emitter cannot round-trip.
//
// Two conditions, both necessary:
//
//   - s contains a line break. Without one the encoder never reaches for a
//     block scalar at all, and its plain/single-quoted/double-quoted choices
//     all read back exactly — so forcing a style here would change the look of
//     ordinary values (`' a'` becoming `" a"`) to fix nothing;
//   - s BEGINS with a space, a tab or a newline. Only the first character
//     decides the indentation a reader infers for a block scalar; everything
//     after it is content.
func needsRoundTripQuoting(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	if !strings.ContainsRune(s, '\n') {
		return false
	}
	switch s[0] {
	case ' ', '\t', '\n':
		return true
	default:
		return false
	}
}

// unmarkRoundTripScalars strips roundTripMarker back off every scalar in n and
// pins those scalars to the double-quoted style, which has no first-line rule
// for the emitter to get wrong.
func unmarkRoundTripScalars(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && strings.HasPrefix(n.Value, roundTripMarker) {
		n.Value = strings.TrimPrefix(n.Value, roundTripMarker)
		n.Tag = strTag
		n.Style = yaml.DoubleQuotedStyle
	}
	for _, child := range n.Content {
		unmarkRoundTripScalars(child)
	}
}

// forceRoundTrippableScalars applies the same rule to an already-decoded node
// tree, for the one path that re-emits a document instead of building one from
// a Go value: SplitWorkspaceEnvelopeDocuments.
//
// It is needed there because that function's re-marshal is a SECOND style
// choice, made on nodes rather than on a value, so a document that parsed on
// the way in could still come out unapplicable — `boid workspace apply` turning
// a valid file into a 400 from its own daemon. A hand-written document is the
// reachable case: an explicit `|2` indicator lets an operator legally write a
// tab-indented first line, which yaml.v3 then re-emits without the indicator.
//
// Walking every scalar rather than looking up spec.init_script by path is the
// same decision as everywhere else in this file: the rule is a property of the
// emitter, not of this schema. Non-string tags (`!!binary` above all) are left
// alone; an already double-quoted scalar has nothing to gain.
func forceRoundTrippableScalars(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && scalarStyleWouldNotRoundTrip(n) {
		n.Style = yaml.DoubleQuotedStyle
	}
	for _, child := range n.Content {
		forceRoundTrippableScalars(child)
	}
}

func scalarStyleWouldNotRoundTrip(n *yaml.Node) bool {
	if n.Style == yaml.DoubleQuotedStyle {
		return false
	}
	switch n.Tag {
	case "", strTag:
	default:
		return false
	}
	return needsRoundTripQuoting(n.Value)
}

// strTag is yaml.v3's resolved tag for a plain string node. Spelled out here
// rather than imported because the constant is unexported in that package.
const strTag = "!!str"

// diffYAMLValues reports the first place want and got disagree on a VALUE, or
// "" when they carry the same document. Styles, comments and line/column
// bookkeeping are deliberately not compared: the question it answers is whether
// a reader would get the same thing back, not whether the two trees are
// identical objects.
func diffYAMLValues(path string, want, got *yaml.Node) string {
	where := path
	if where == "" {
		where = "document"
	}
	if want == nil || got == nil {
		if want == got {
			return ""
		}
		return fmt.Sprintf("%s: one side is absent", where)
	}
	if want.Kind != got.Kind {
		return fmt.Sprintf("%s: node kind changed", where)
	}
	if want.Kind == yaml.ScalarNode {
		if want.Value != got.Value {
			return fmt.Sprintf("%s: %q reads back as %q", where, want.Value, got.Value)
		}
		if want.Tag != got.Tag {
			return fmt.Sprintf("%s: tag %s reads back as %s", where, want.Tag, got.Tag)
		}
		return ""
	}
	if len(want.Content) != len(got.Content) {
		return fmt.Sprintf("%s: %d entries read back as %d", where, len(want.Content), len(got.Content))
	}
	for i := range want.Content {
		childPath := path
		switch {
		case want.Kind == yaml.SequenceNode:
			childPath = fmt.Sprintf("%s[%d]", path, i)
		case want.Kind == yaml.MappingNode && i%2 == 1:
			childPath = strings.TrimPrefix(path+"."+want.Content[i-1].Value, ".")
		case want.Kind == yaml.MappingNode:
			childPath = strings.TrimPrefix(path+".<key>", ".")
		}
		if d := diffYAMLValues(childPath, want.Content[i], got.Content[i]); d != "" {
			return d
		}
	}
	return ""
}

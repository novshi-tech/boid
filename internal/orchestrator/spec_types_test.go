package orchestrator_test

import (
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

// TestBindMount_UnmarshalYAML_ShortForm verifies that a bare string
// is accepted as shorthand for {Source: <string>}.
// Hand-written kit.yaml and earlier boid-kit-init templates used this form;
// previously yaml.v3 rejected it with
// "cannot unmarshal !!str into orchestrator.BindMount" which cascaded into
// project meta hydration falling back to raw meta.
func TestBindMount_UnmarshalYAML_ShortForm(t *testing.T) {
	var got projectspec.BindMount
	if err := yaml.Unmarshal([]byte(`/host/path`), &got); err != nil {
		t.Fatalf("unmarshal short form: %v", err)
	}
	want := projectspec.BindMount{Source: "/host/path"}
	if got != want {
		t.Fatalf("short form: got %+v, want %+v", got, want)
	}
}

// TestBindMount_UnmarshalYAML_StructForm verifies that the canonical
// struct form still decodes every field as before.
func TestBindMount_UnmarshalYAML_StructForm(t *testing.T) {
	yamlBody := `
source: /host/path
target: /sandbox/path
mode: rw
is_file: true
optional: true
`
	var got projectspec.BindMount
	if err := yaml.Unmarshal([]byte(yamlBody), &got); err != nil {
		t.Fatalf("unmarshal struct form: %v", err)
	}
	want := projectspec.BindMount{
		Source:   "/host/path",
		Target:   "/sandbox/path",
		Mode:     "rw",
		IsFile:   true,
		Optional: true,
	}
	if got != want {
		t.Fatalf("struct form: got %+v, want %+v", got, want)
	}
}

// TestBindMount_UnmarshalYAML_MixedList verifies that a sequence may
// contain both forms; this is the realistic kit.yaml shape where most
// entries are read-only short strings and a few entries need mode: rw.
func TestBindMount_UnmarshalYAML_MixedList(t *testing.T) {
	yamlBody := `
- /usr/local/go
- source: /home/u/.cache/uv
  mode: rw
- /home/u/go/bin
`
	var got []projectspec.BindMount
	if err := yaml.Unmarshal([]byte(yamlBody), &got); err != nil {
		t.Fatalf("unmarshal mixed list: %v", err)
	}
	want := []projectspec.BindMount{
		{Source: "/usr/local/go"},
		{Source: "/home/u/.cache/uv", Mode: "rw"},
		{Source: "/home/u/go/bin"},
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestHostCommands_UnmarshalYAML_RejectRules verifies that the map form
// accepts the reject: rule list (match + reason) alongside allow/deny.
func TestHostCommands_UnmarshalYAML_RejectRules(t *testing.T) {
	yamlBody := `
gh:
  allow: [pr, issue]
  reject:
    - match: "*--body-file*"
      reason: 'use --body "$(cat <file>)" instead'
`
	var got projectspec.HostCommands
	if err := yaml.Unmarshal([]byte(yamlBody), &got); err != nil {
		t.Fatalf("unmarshal reject rules: %v", err)
	}
	gh := got["gh"]
	if len(gh.Reject) != 1 {
		t.Fatalf("expected 1 reject rule, got %+v", gh.Reject)
	}
	want := projectspec.RejectRule{Match: "*--body-file*", Reason: `use --body "$(cat <file>)" instead`}
	if gh.Reject[0] != want {
		t.Fatalf("reject rule: got %+v, want %+v", gh.Reject[0], want)
	}
}

// TestHostCommands_UnmarshalYAML_ListFormStillWorks verifies that the
// shorthand list form is unaffected by the reject vocabulary addition.
func TestHostCommands_UnmarshalYAML_ListFormStillWorks(t *testing.T) {
	var got projectspec.HostCommands
	if err := yaml.Unmarshal([]byte(`[gh, aws]`), &got); err != nil {
		t.Fatalf("unmarshal list form: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 commands, got %+v", got)
	}
	for _, name := range []string{"gh", "aws"} {
		spec, ok := got[name]
		if !ok {
			t.Fatalf("missing %q in %+v", name, got)
		}
		if len(spec.Reject) != 0 {
			t.Fatalf("list form must yield empty reject rules, got %+v", spec.Reject)
		}
	}
}

// TestHostCommandSpec_ToCommandDef_RejectPassthrough verifies that reject
// rules survive the HostCommandSpec → CommandDef conversion.
func TestHostCommandSpec_ToCommandDef_RejectPassthrough(t *testing.T) {
	spec := projectspec.HostCommandSpec{
		Allow: []string{"pr"},
		Reject: []projectspec.RejectRule{
			{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
			{Match: "* --web*", Reason: "no browser in the sandbox"},
		},
	}
	def := spec.ToCommandDef("gh")
	if len(def.RejectRules) != 2 {
		t.Fatalf("expected 2 reject rules, got %+v", def.RejectRules)
	}
	if def.RejectRules[0] != spec.Reject[0] || def.RejectRules[1] != spec.Reject[1] {
		t.Fatalf("reject rules not passed through: got %+v, want %+v", def.RejectRules, spec.Reject)
	}
}

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

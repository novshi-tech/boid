package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot creates a fresh cobra root with --output flag for unit testing,
// without touching the global rootCmd.
func newTestRoot(t *testing.T, format string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().StringP("output", "o", "plain", "output format")
	if format != "plain" {
		if err := root.PersistentFlags().Set("output", format); err != nil {
			t.Fatalf("set output flag: %v", err)
		}
	}
	sub := &cobra.Command{Use: "sub"}
	root.AddCommand(sub)
	return sub
}

func TestGetOutputFormat_Default(t *testing.T) {
	cmd := newTestRoot(t, "plain")
	got := getOutputFormat(cmd)
	if got != "plain" {
		t.Errorf("default output format = %q, want \"plain\"", got)
	}
}

func TestGetOutputFormat_JSON(t *testing.T) {
	cmd := newTestRoot(t, "json")
	got := getOutputFormat(cmd)
	if got != "json" {
		t.Errorf("output format = %q, want \"json\"", got)
	}
}

func TestGetOutputFormat_YAML(t *testing.T) {
	cmd := newTestRoot(t, "yaml")
	got := getOutputFormat(cmd)
	if got != "yaml" {
		t.Errorf("output format = %q, want \"yaml\"", got)
	}
}

func TestRenderOutput_Plain_CallsPlainFn(t *testing.T) {
	cmd := newTestRoot(t, "plain")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	plainCalled := false
	err := renderOutput(cmd, map[string]string{"key": "value"}, func() error {
		plainCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("renderOutput: %v", err)
	}
	if !plainCalled {
		t.Error("expected plainFn to be called for plain output")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output to cmd writer for plain, got: %q", buf.String())
	}
}

func TestRenderOutput_JSON(t *testing.T) {
	cmd := newTestRoot(t, "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	type testData struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	err := renderOutput(cmd, testData{Name: "Alice", Age: 30}, func() error { return nil })
	if err != nil {
		t.Fatalf("renderOutput: %v", err)
	}
	got := buf.String()
	for _, want := range []string{`"name"`, `"Alice"`, `"age"`, `30`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON output missing %q: %q", want, got)
		}
	}
}

func TestRenderOutput_JSON_DoesNotCallPlainFn(t *testing.T) {
	cmd := newTestRoot(t, "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	plainCalled := false
	_ = renderOutput(cmd, "value", func() error {
		plainCalled = true
		return nil
	})
	if plainCalled {
		t.Error("plainFn must not be called for json output")
	}
}

func TestRenderOutput_YAML(t *testing.T) {
	cmd := newTestRoot(t, "yaml")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	type testData struct {
		Name string `yaml:"name"`
		Age  int    `yaml:"age"`
	}
	err := renderOutput(cmd, testData{Name: "Bob", Age: 25}, func() error { return nil })
	if err != nil {
		t.Fatalf("renderOutput: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "name: Bob") {
		t.Errorf("YAML output missing 'name: Bob': %q", got)
	}
	if !strings.Contains(got, "age: 25") {
		t.Errorf("YAML output missing 'age: 25': %q", got)
	}
}

func TestRenderOutput_YAML_DoesNotCallPlainFn(t *testing.T) {
	cmd := newTestRoot(t, "yaml")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	plainCalled := false
	_ = renderOutput(cmd, "value", func() error {
		plainCalled = true
		return nil
	})
	if plainCalled {
		t.Error("plainFn must not be called for yaml output")
	}
}

func TestRenderOutput_JSON_NilValue(t *testing.T) {
	cmd := newTestRoot(t, "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	err := renderOutput(cmd, nil, func() error { return nil })
	if err != nil {
		t.Fatalf("renderOutput nil: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "null" {
		t.Errorf("JSON nil value: got %q, want \"null\"", got)
	}
}

func TestRenderOutput_JSON_EmptySlice(t *testing.T) {
	cmd := newTestRoot(t, "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	err := renderOutput(cmd, []string{}, func() error { return nil })
	if err != nil {
		t.Fatalf("renderOutput: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Errorf("JSON empty slice: got %q, want \"[]\"", got)
	}
}

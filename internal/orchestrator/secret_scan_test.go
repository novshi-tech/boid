package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestScanSecretsBytes_Clean(t *testing.T) {
	yaml := `
meta:
  name: node
  description: Node.js toolchain
host_commands:
  node:
    path: /usr/local/bin/node
    args: ["--version"]
env:
  NODE_ENV: production
  NPM_TOKEN: secret:npm_token
additional_bindings:
  - /home/nosen/.volta/bin
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %d: %v", len(findings), findings)
	}
}

func TestScanSecretsBytes_Empty(t *testing.T) {
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no findings on empty input, got %d", len(findings))
	}
}

func TestScanSecretsBytes_InvalidYAML(t *testing.T) {
	_, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte("foo: [unterminated"))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestScanSecretsBytes_HighEntropyInEnvValue(t *testing.T) {
	yaml := `
env:
  NPM_TOKEN: abcdef1234567890ABCDEF1234567890XYZ
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %v", len(findings), findings)
	}
	if findings[0].Kind != "high-entropy" {
		t.Errorf("expected Kind=high-entropy, got %q", findings[0].Kind)
	}
	if findings[0].Path != "kit.yaml" {
		t.Errorf("expected Path=kit.yaml, got %q", findings[0].Path)
	}
	if findings[0].Line < 2 {
		t.Errorf("expected Line>=2, got %d", findings[0].Line)
	}
	// Snippet must be redacted: must not contain the full raw token.
	if strings.Contains(findings[0].Snippet, "abcdef1234567890ABCDEF1234567890XYZ") {
		t.Errorf("snippet leaks raw value: %q", findings[0].Snippet)
	}
}

func TestScanSecretsBytes_HighEntropyInSequence(t *testing.T) {
	yaml := `
host_commands:
  curl:
    path: /usr/bin/curl
    args:
      - "--header"
      - "Authorization: Bearer aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %v", len(findings), findings)
	}
	if findings[0].Kind != "high-entropy" {
		t.Errorf("expected Kind=high-entropy, got %q", findings[0].Kind)
	}
}

func TestScanSecretsBytes_LiteralMarkers(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		pattern string
	}{
		{"password", `--password=hunter2 short`, "password="},
		{"token", `Authorization Token=xyz short`, "token="},
		{"secret", `secret=abc short`, "secret="},
		{"api_key", `--api_key=abc short`, "api_key="},
		{"apikey", `apikey=abc short`, "apikey="},
		{"PASSWORD case-insensitive", `--PASSWORD=hunter2 short`, "password="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "args: [\"" + tc.value + "\"]\n"
			findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(doc))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(findings) == 0 {
				t.Fatalf("expected literal-marker finding, got none")
			}
			var found bool
			for _, f := range findings {
				if f.Kind == "literal-marker" && f.Pattern == tc.pattern {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected pattern %q in findings: %v", tc.pattern, findings)
			}
		})
	}
}

func TestScanSecretsBytes_WhitelistedSecretRef(t *testing.T) {
	yaml := `
env:
  NPM_TOKEN: secret:npm_token
  GITHUB_TOKEN: secret:github_pat
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("secret: refs should be whitelisted, got %d findings: %v", len(findings), findings)
	}
}

func TestScanSecretsBytes_WhitelistedEnvVar(t *testing.T) {
	yaml := `
env:
  HOME: ${HOME}
  ANOTHER: ${SOME_VAR_NAME}
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("${VAR} refs should be whitelisted, got %d findings: %v", len(findings), findings)
	}
}

func TestScanSecretsBytes_WhitelistedPaths(t *testing.T) {
	yaml := `
additional_bindings:
  - /home/nosen/.local/share/some_long_directory_name_that_would_otherwise_trigger
  - /usr/local/bin/node
host_commands:
  node:
    path: /home/nosen/.volta/tools/image/node/20.0.0/bin/node
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("absolute paths should be whitelisted, got %d findings: %v", len(findings), findings)
	}
}

func TestScanSecretsBytes_WhitelistedDigest(t *testing.T) {
	yaml := `
images:
  - sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  - sha512:cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("digest prefixes should be whitelisted, got %d findings: %v", len(findings), findings)
	}
}

func TestScanSecretsBytes_KeyNamesNotFlagged(t *testing.T) {
	// Mapping keys like "password" or "api_key" are field names, not values.
	// They must never trigger the literal-marker rule (which only checks
	// for a marker followed by "=<non-space>" inside a value).
	yaml := `
env:
  password: short
  api_key: short
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("plain short values under sus key names should not be flagged, got %d: %v", len(findings), findings)
	}
}

func TestScanSecretsBytes_MultipleFindingsAggregated(t *testing.T) {
	yaml := `
env:
  ONE: abcdef1234567890ABCDEF1234567890XYZQ
  TWO: --password=foo
  THREE: short
`
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %v", len(findings), findings)
	}
	var sawEntropy, sawMarker bool
	for _, f := range findings {
		if f.Kind == "high-entropy" {
			sawEntropy = true
		}
		if f.Kind == "literal-marker" {
			sawMarker = true
		}
	}
	if !sawEntropy || !sawMarker {
		t.Errorf("expected both kinds, got entropy=%v marker=%v", sawEntropy, sawMarker)
	}
}

func TestScanSecretsBytes_SnippetRedaction(t *testing.T) {
	// Long value: first4 + ... + last4
	long := "abcdefghijklmnopqrstuvwxyz0123456789"
	doc := "env: { TOKEN: " + long + " }\n"
	findings, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	snip := findings[0].Snippet
	if !strings.Contains(snip, "...") {
		t.Errorf("expected ellipsis in long-value snippet, got %q", snip)
	}
	if strings.Contains(snip, long) {
		t.Errorf("snippet leaks raw value: %q", snip)
	}
	// Short value: just length annotation.
	short := "short"
	doc2 := "env: { X: \"--password=" + short + "\" }\n"
	findings2, err := orchestrator.ScanSecretsBytes("kit.yaml", []byte(doc2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings2) == 0 {
		t.Fatalf("expected at least 1 finding")
	}
}

func TestScanSecretsBytes_StringMethod(t *testing.T) {
	yaml := "env: { TOKEN: abcdef1234567890ABCDEF1234567890XYZQ }\n"
	findings, err := orchestrator.ScanSecretsBytes("/tmp/kit.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	got := findings[0].String()
	if !strings.HasPrefix(got, "/tmp/kit.yaml:") {
		t.Errorf("String() should start with path, got %q", got)
	}
	if !strings.Contains(got, "high-entropy") {
		t.Errorf("String() should contain Kind, got %q", got)
	}
}

func TestScanSecretsFile_ReadsAndScans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kit.yaml")
	contents := []byte("env: { TOKEN: abcdef1234567890ABCDEF1234567890XYZQ }\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	findings, err := orchestrator.ScanSecretsFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %v", len(findings), findings)
	}
	if findings[0].Path != path {
		t.Errorf("expected Path=%q, got %q", path, findings[0].Path)
	}
}

func TestScanSecretsFile_MissingPath(t *testing.T) {
	_, err := orchestrator.ScanSecretsFile("/no/such/file/here.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}


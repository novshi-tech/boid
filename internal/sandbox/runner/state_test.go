package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// writeJSON marshals v to path with the same default encoding the dispatcher's
// PrepareSandbox uses, so the runner's readSpec round-trips it exactly.
func writeJSON(t *testing.T, path string, v any) error {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func TestRedactEnv(t *testing.T) {
	in := map[string]string{
		"HOME":               "/home/x",
		"PATH":               "/bin:/usr/bin",
		"TERM":               "xterm-256color",
		"LC_ALL":             "C.UTF-8",
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": "/run/boid/broker.sock",
		"BOID_BROKER_TOKEN":  "super-secret-token",
		"ANTHROPIC_API_KEY":  "sk-secret",
		"GITHUB_TOKEN":       "ghp_secret",
		"RANDOM_THING":       "value",
	}
	out := redactEnv(in)

	verbatim := []string{"HOME", "PATH", "TERM", "LC_ALL", "BOID_JOB_ID", "BOID_BROKER_SOCKET"}
	for _, k := range verbatim {
		if out[k] != in[k] {
			t.Errorf("expected %s kept verbatim, got %q", k, out[k])
		}
	}
	redacted := []string{"BOID_BROKER_TOKEN", "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "RANDOM_THING"}
	for _, k := range redacted {
		if out[k] != "<redacted>" {
			t.Errorf("expected %s redacted, got %q", k, out[k])
		}
	}
	// No secret value should leak anywhere in the redacted map.
	for k, v := range out {
		if strings.Contains(v, "secret") {
			t.Errorf("secret leaked in redacted env: %s=%q", k, v)
		}
	}
}

func TestBuildSpecDump_RedactsEnvAndCapturesLayout(t *testing.T) {
	spec := sandbox.Spec{
		ID:        "job-9",
		Argv:      []string{"/bin/agent"},
		WorkDir:   "/work",
		RootDir:   "/tmp/boid-root-abc",
		ProxyPort: 8080,
		Env: map[string]string{
			"BOID_BROKER_TOKEN": "tok",
			"HOME":              "/home/x",
		},
	}
	dump := buildSpecDump(spec, []string{"pasta", "--config-net"})

	if dump.Env["BOID_BROKER_TOKEN"] != "<redacted>" {
		t.Errorf("token must be redacted in spec dump, got %q", dump.Env["BOID_BROKER_TOKEN"])
	}
	if dump.Env["HOME"] != "/home/x" {
		t.Errorf("HOME must be kept, got %q", dump.Env["HOME"])
	}
	if dump.PivotRoot != "/tmp/boid-root-abc" {
		t.Errorf("PivotRoot = %q, want root dir", dump.PivotRoot)
	}
	// ProxyPort > 0 means nft rules must be present in the dump.
	if len(dump.NFTRules) == 0 {
		t.Error("expected nft rules in dump when ProxyPort > 0")
	}
	// Base mounts (/usr etc.) must be captured.
	foundUsr := false
	for _, m := range dump.Mounts {
		if m.Target == "/usr" {
			foundUsr = true
		}
	}
	if !foundUsr {
		t.Error("expected /usr base mount in dump")
	}
}

func TestState_NDJSONAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-state.json")

	st := OpenState(path)
	if st == nil {
		t.Fatal("OpenState returned nil for valid path")
	}
	st.Spec("outer", sandbox.Spec{ID: "job-1", Env: map[string]string{"X": "secret"}}, []string{"pasta"})
	st.OK("inner", "nft")
	st.Fail("inner-child", "pivot-root", os.ErrPermission)
	st.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer f.Close()

	var lines []stateLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l stateLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("each line must be valid JSON: %v (line=%q)", err, sc.Text())
		}
		lines = append(lines, l)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON lines, got %d", len(lines))
	}
	if lines[0].Spec == nil {
		t.Error("first line must be the spec dump")
	} else if lines[0].Spec.Env["X"] != "<redacted>" {
		t.Errorf("spec dump env must be redacted, got %q", lines[0].Spec.Env["X"])
	}
	if lines[1].Phase != "nft" || lines[1].Status != "ok" {
		t.Errorf("line 1 = %+v, want nft/ok", lines[1])
	}
	if lines[2].Status != "error" || lines[2].Phase != "pivot-root" {
		t.Errorf("line 2 = %+v, want pivot-root/error", lines[2])
	}
}

func TestState_NilSafe(t *testing.T) {
	// OpenState("") returns nil; all methods must be no-ops.
	var st *State = OpenState("")
	st.Spec("outer", sandbox.Spec{}, nil)
	st.OK("x", "y")
	st.Fail("x", "y", nil)
	st.Close()
}

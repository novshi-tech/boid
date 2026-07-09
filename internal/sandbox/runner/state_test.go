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

func TestRedactCloneURLToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "well-formed gateway URL",
			in:   "http://10.0.2.2:12345/j/deadbeefdeadbeef/github.com/owner/repo.git",
			want: "http://10.0.2.2:12345/j/<redacted>/github.com/owner/repo.git",
		},
		{
			name: "no /j/ marker returned unchanged",
			in:   "https://github.com/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "trailing /j/ with nothing after it returned unchanged",
			in:   "http://10.0.2.2:1/j/",
			want: "http://10.0.2.2:1/j/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactCloneURLToken(tc.in)
			if got != tc.want {
				t.Errorf("redactCloneURLToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.in != "" && strings.Contains(tc.in, "deadbeefdeadbeef") && strings.Contains(got, "deadbeefdeadbeef") {
				t.Errorf("token leaked into redacted output: %q", got)
			}
		})
	}
}

// TestBuildSpecDump_CloneRedactsTokenAndCapturesDeclaration is the
// runner-state.json guard for docs/plans/git-gateway-cutover.md PR5's token
// redact heads-up (PR4 review): the clone URL's job token must never appear
// verbatim in the diagnostic dump, while the rest of the branch declaration
// (used to diagnose a failed clone/resolve) is preserved.
func TestBuildSpecDump_CloneRedactsTokenAndCapturesDeclaration(t *testing.T) {
	const token = "secrettoken1234567890"
	spec := sandbox.Spec{
		ID: "job-clone",
		Clone: sandbox.CloneSpec{
			Enabled:      true,
			URL:          "http://10.0.2.2:9/j/" + token + "/github.com/owner/repo.git",
			ReferenceDir: "/mnt/refs/self.git",
			TargetDir:    "/workspace",
			RealGitBin:   "/run/boid/real-git",
			Branch:       "boid/abcd1234",
			BaseBranch:   "main",
			ForkPoint:    "boid/parent12",
		},
	}
	dump := buildSpecDump(spec, nil)

	if dump.Clone == nil {
		t.Fatal("expected non-nil Clone dump when spec.Clone.Enabled")
	}
	if strings.Contains(dump.Clone.URL, token) {
		t.Errorf("job token leaked into runner-state.json dump: %q", dump.Clone.URL)
	}
	if dump.Clone.TargetDir != "/workspace" || dump.Clone.Branch != "boid/abcd1234" || dump.Clone.BaseBranch != "main" {
		t.Errorf("clone dump did not preserve declaration fields: %+v", dump.Clone)
	}

	// The whole marshalled dump must not contain the token either (belt and
	// suspenders: proves no other field aliases spec.Clone.URL verbatim).
	encoded, err := json.Marshal(dump)
	if err != nil {
		t.Fatalf("marshal dump: %v", err)
	}
	if strings.Contains(string(encoded), token) {
		t.Errorf("job token leaked somewhere in the marshalled spec dump: %s", encoded)
	}
}

func TestBuildSpecDump_CloneDisabledOmitsCloneField(t *testing.T) {
	spec := sandbox.Spec{ID: "job-no-clone"}
	dump := buildSpecDump(spec, nil)
	if dump.Clone != nil {
		t.Errorf("expected nil Clone dump when spec.Clone.Enabled is false, got %+v", dump.Clone)
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

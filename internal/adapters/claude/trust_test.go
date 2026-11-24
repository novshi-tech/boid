package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readClaudeJSON(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTrustWorkspace_CreatesFile(t *testing.T) {
	home := t.TempDir()
	if err := trustWorkspace(home, "/workspace/demo"); err != nil {
		t.Fatal(err)
	}
	got := readClaudeJSON(t, home)
	p := got["projects"].(map[string]any)["/workspace/demo"].(map[string]any)
	if p["hasTrustDialogAccepted"] != true {
		t.Fatalf("not trusted: %v", got)
	}
	fi, _ := os.Stat(filepath.Join(home, ".claude.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestTrustWorkspace_KeepsOtherSettings(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	orig := `{"oauthAccount":{"x":1},"numStartups":7,"projects":{"/workspace/other":{"hasTrustDialogAccepted":true,"allowedTools":["Bash"]},"/workspace/demo":{"lastCost":1.5}}}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := trustWorkspace(home, "/workspace/demo"); err != nil {
		t.Fatal(err)
	}
	got := readClaudeJSON(t, home)
	if got["numStartups"] != float64(7) || got["oauthAccount"].(map[string]any)["x"] != float64(1) {
		t.Fatalf("top-level settings lost: %v", got)
	}
	projects := got["projects"].(map[string]any)
	if projects["/workspace/other"].(map[string]any)["allowedTools"] == nil {
		t.Fatalf("other project lost: %v", projects)
	}
	demo := projects["/workspace/demo"].(map[string]any)
	if demo["hasTrustDialogAccepted"] != true || demo["lastCost"] != 1.5 {
		t.Fatalf("demo entry = %v", demo)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
}

func TestTrustWorkspace_AlreadyTrustedLeavesFileAlone(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	orig := `{"projects":{"/workspace/demo":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := trustWorkspace(home, "/workspace/demo"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != orig {
		t.Fatalf("file rewritten: %s", raw)
	}
}

func TestTrustWorkspace_BrokenFileIsAnError(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := trustWorkspace(home, "/workspace/demo"); err == nil {
		t.Fatal("want error")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{not json" {
		t.Fatalf("broken file overwritten: %s", raw)
	}
}

func TestClaudeHome_PrefersRunContextEnv(t *testing.T) {
	if got := claudeHome(map[string]string{"HOME": "/home/job"}); got != "/home/job" {
		t.Fatalf("claudeHome = %q", got)
	}
	t.Setenv("HOME", "/home/inherited")
	if got := claudeHome(nil); got != "/home/inherited" {
		t.Fatalf("claudeHome = %q", got)
	}
}

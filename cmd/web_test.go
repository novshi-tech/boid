package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebCommandRegistration(t *testing.T) {
	subcommands := map[string]bool{}
	for _, c := range webCmd.Commands() {
		subcommands[c.Name()] = true
	}
	for _, name := range []string{"pair", "devices", "revoke", "revoke-all", "set-url"} {
		if !subcommands[name] {
			t.Errorf("webCmd missing subcommand %q", name)
		}
	}
}

func TestWebCmdRegisteredOnRoot(t *testing.T) {
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "web" {
			found = true
			break
		}
	}
	if !found {
		t.Error("rootCmd does not have 'web' subcommand")
	}
}

func TestWebRevokeRequiresArg(t *testing.T) {
	if err := webRevokeCmd.Args(webRevokeCmd, []string{}); err == nil {
		t.Error("revoke: expected error for missing id arg")
	}
	if err := webRevokeCmd.Args(webRevokeCmd, []string{"abc"}); err != nil {
		t.Errorf("revoke: unexpected error for single arg: %v", err)
	}
}

func TestWebSetURLRequiresArg(t *testing.T) {
	if err := webSetURLCmd.Args(webSetURLCmd, []string{}); err == nil {
		t.Error("set-url: expected error for missing URL arg")
	}
	if err := webSetURLCmd.Args(webSetURLCmd, []string{"https://example.com"}); err != nil {
		t.Errorf("set-url: unexpected error for single arg: %v", err)
	}
}

func TestWebSetURLSkipsAutostart(t *testing.T) {
	if webSetURLCmd.Annotations[annotationSkipAutostart] != "skip" {
		t.Error("set-url must have annotationSkipAutostart=skip")
	}
}

func TestRunWebSetURL_WritesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	var buf strings.Builder
	webSetURLCmd.SetOut(&buf)

	if err := runWebSetURL(webSetURLCmd, []string{"https://boid.example.com"}); err != nil {
		t.Fatalf("runWebSetURL: %v", err)
	}

	configPath := filepath.Join(dir, "boid", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "https://boid.example.com") {
		t.Errorf("config missing URL:\n%s", string(data))
	}
	if !strings.Contains(buf.String(), "web.public_url") {
		t.Errorf("output missing 'web.public_url': %q", buf.String())
	}
}

func TestRunWebSetURL_PreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "gc:\n  enabled: false\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runWebSetURL(webSetURLCmd, []string{"https://new.example.com"}); err != nil {
		t.Fatalf("runWebSetURL: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "enabled: false") {
		t.Errorf("existing gc.enabled lost:\n%s", content)
	}
	if !strings.Contains(content, "https://new.example.com") {
		t.Errorf("new URL not written:\n%s", content)
	}
}

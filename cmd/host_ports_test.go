//go:build linux

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func TestLoadHostPorts_MissingFile_Defaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := loadHostPorts()
	if err != nil {
		t.Fatalf("loadHostPorts: %v", err)
	}
	if got.CLI != 8442 || got.Web != 8080 {
		t.Errorf("loadHostPorts() = %+v, want CLI=8442 Web=8080", got)
	}
	if addr := got.cliAddr(); addr != "127.0.0.1:8442" {
		t.Errorf("cliAddr() = %q, want 127.0.0.1:8442", addr)
	}
}

func TestSaveHostPorts_RoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := saveHostPorts(hostPorts{CLI: 9442, Web: 9080}); err != nil {
		t.Fatalf("saveHostPorts: %v", err)
	}
	got, err := loadHostPorts()
	if err != nil {
		t.Fatalf("loadHostPorts: %v", err)
	}
	if got.CLI != 9442 || got.Web != 9080 {
		t.Errorf("loadHostPorts() = %+v, want CLI=9442 Web=9080", got)
	}
}

func TestLoadHostPorts_PartialFile_FillsDefaults(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	writeHostPortsFile(t, cfg, `{"cli_port": 9442}`)

	got, err := loadHostPorts()
	if err != nil {
		t.Fatalf("loadHostPorts: %v", err)
	}
	if got.CLI != 9442 || got.Web != 8080 {
		t.Errorf("loadHostPorts() = %+v, want CLI=9442 Web=8080", got)
	}
}

func TestLoadHostPorts_InvalidFile_ErrorNamesPath(t *testing.T) {
	for name, body := range map[string]string{
		"out of range": `{"cli_port": 70000}`,
		"negative":     `{"web_port": -1}`,
		"not json":     `cli_port=9442`,
		"same port":    `{"cli_port": 9000, "web_port": 9000}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfg)
			writeHostPortsFile(t, cfg, body)

			_, err := loadHostPorts()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), filepath.Join(cfg, "boid", hostPortsFileName)) {
				t.Errorf("error should name the file, got %q", err.Error())
			}
		})
	}
}

func TestHostPorts_ComposeEnv(t *testing.T) {
	got := hostPorts{CLI: 9442, Web: 9080}.composeEnv()
	for _, want := range []string{"BOID_CLI_PORT=9442", "BOID_WEB_PORT=9080"} {
		if !slices.Contains(got, want) {
			t.Errorf("composeEnv() = %v, want %q", got, want)
		}
	}
}

func newPortFlagsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "start"}
	var cliPort, webPort int
	cmd.Flags().IntVar(&cliPort, "cli-port", 0, "")
	cmd.Flags().IntVar(&webPort, "web-port", 0, "")
	return cmd
}

func TestResolveStartHostPorts_FlagsPersist(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newPortFlagsCmd()
	if err := cmd.Flags().Set("cli-port", "9442"); err != nil {
		t.Fatal(err)
	}

	got, err := resolveStartHostPorts(cmd)
	if err != nil {
		t.Fatalf("resolveStartHostPorts: %v", err)
	}
	if got.CLI != 9442 || got.Web != 8080 {
		t.Errorf("resolveStartHostPorts() = %+v, want CLI=9442 Web=8080", got)
	}

	// A later command without the flag must still see the saved port.
	loaded, err := loadHostPorts()
	if err != nil {
		t.Fatalf("loadHostPorts: %v", err)
	}
	if loaded != got {
		t.Errorf("loadHostPorts() after start = %+v, want %+v", loaded, got)
	}
}

func TestResolveStartHostPorts_NoFlags_KeepsSavedPorts(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := saveHostPorts(hostPorts{CLI: 9442, Web: 9080}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveStartHostPorts(newPortFlagsCmd())
	if err != nil {
		t.Fatalf("resolveStartHostPorts: %v", err)
	}
	if got.CLI != 9442 || got.Web != 9080 {
		t.Errorf("resolveStartHostPorts() = %+v, want the saved CLI=9442 Web=9080", got)
	}
}

func TestResolveStartHostPorts_InvalidFlag_RefusesWithoutSaving(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	cmd := newPortFlagsCmd()
	if err := cmd.Flags().Set("web-port", "8442"); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveStartHostPorts(cmd); err == nil {
		t.Fatal("expected an error when --web-port collides with the CLI port")
	}
	if _, err := os.Stat(filepath.Join(cfg, "boid", hostPortsFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no %s to be written for a rejected flag, stat err = %v", hostPortsFileName, err)
	}
}

func writeHostPortsFile(t *testing.T, configHome, body string) {
	t.Helper()
	dir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostPortsFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseHostPortFlagsWithForeground(t *testing.T) {
	cmd := newPortFlagsCmd()
	if err := refuseHostPortFlagsWithForeground(cmd); err != nil {
		t.Fatalf("expected no error without the flags, got %v", err)
	}
	if err := cmd.Flags().Set("web-port", "9080"); err != nil {
		t.Fatal(err)
	}
	err := refuseHostPortFlagsWithForeground(cmd)
	if err == nil || !strings.Contains(err.Error(), "--web-port") {
		t.Errorf("expected an error naming --web-port, got %v", err)
	}
}

func TestRunStart_Foreground_WebPortFlag_RefusesBeforeStarting(t *testing.T) {
	startForeground = true
	t.Cleanup(func() { startForeground = false })
	t.Cleanup(func() {
		startHostWebPort = 0
		startCmd.Flags().Lookup("web-port").Changed = false
	})
	if err := startCmd.Flags().Set("web-port", "9080"); err != nil {
		t.Fatal(err)
	}

	err := runStart(startCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--web-port") {
		t.Fatalf("expected a refusal naming --web-port, got %v", err)
	}
}

func TestResolveHostModeClientNoAutostart_DialsSavedCLIPort(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping-from-test" {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ports := hostPortsForAddr(t, ts.Listener.Addr().String())
	if err := saveHostPorts(ports); err != nil {
		t.Fatal(err)
	}

	c, err := resolveHostModeClientNoAutostart(context.Background())
	if err != nil {
		t.Fatalf("resolveHostModeClientNoAutostart: %v", err)
	}
	if err := c.Do(http.MethodGet, "/api/ping-from-test", nil, nil); err != nil {
		t.Fatalf("request through client: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("the client did not reach the daemon on the saved CLI port %d", ports.CLI)
	}
}

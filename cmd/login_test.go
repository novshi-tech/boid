package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/profiles"
	"github.com/spf13/cobra"
)

// withTestDefaultTransport temporarily swaps the process-wide
// http.DefaultTransport for rt, restoring it on test cleanup. client.NewClient
// builds an https-scheme Client with a nil custom transport
// (internal/client/client.go's newHTTPSClient), which falls back to
// http.DefaultTransport at request time — swapping it here is how these
// tests get client.NewClient (a real, unmodified production code path,
// not a test-only seam) to trust an httptest.NewTLSServer's self-signed
// certificate, without needing internal/client to expose any test-only
// constructor. Not run with t.Parallel() (matches this package's existing
// convention of avoiding shared-state races — see root_test.go's
// newProfileTestCmd doc comment).
func withTestDefaultTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func withLoginXDGConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func newLoginTestCmd(t *testing.T, profileFlag, deviceNameFlag, stdin string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "login"}
	cmd.SetContext(context.Background()) // a real Execute() run always has one; a bare literal here does not
	cmd.Flags().String(profiles.ProfileFlagName, "", "")
	cmd.Flags().String(deviceNameFlagName, "", "")
	if profileFlag != "" {
		if err := cmd.Flags().Set(profiles.ProfileFlagName, profileFlag); err != nil {
			t.Fatalf("set --profile: %v", err)
		}
	}
	if deviceNameFlag != "" {
		if err := cmd.Flags().Set(deviceNameFlagName, deviceNameFlag); err != nil {
			t.Fatalf("set --device-name: %v", err)
		}
	}
	cmd.SetIn(strings.NewReader(stdin))
	return cmd
}

// newLogoutTestCmd builds a standalone leaf *cobra.Command suitable for
// calling runLogout directly (bypassing cobra's Execute() lifecycle, same
// rationale as newLoginTestCmd/newProfileTestCmd in root_test.go). logout
// takes its target profile as a positional arg, so unlike login it needs no
// flags — just a non-nil context (runLogout's daemon-revoke path builds a
// bounded context.WithTimeout(cmd.Context(), ...), which panics on a nil
// parent).
func newLogoutTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "logout"}
	cmd.SetContext(context.Background())
	return cmd
}

// --- URL scheme validation (TDD step 6) ---

func TestLogin_RejectsUnixScheme(t *testing.T) {
	withLoginXDGConfigHome(t)
	cmd := newLoginTestCmd(t, "work", "", "CODE-1234\n")
	err := runLogin(cmd, []string{"unix:///run/user/1000/boid.sock"})
	if err == nil {
		t.Fatal("expected an error for a unix:// login URL")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("error should mention the https:// requirement, got %q", err.Error())
	}
}

func TestLogin_RejectsHTTPScheme(t *testing.T) {
	withLoginXDGConfigHome(t)
	cmd := newLoginTestCmd(t, "work", "", "CODE-1234\n")
	err := runLogin(cmd, []string{"http://work.example.com"})
	if err == nil {
		t.Fatal("expected an error for an http:// login URL")
	}
}

// --- profile name derivation (TDD step 5) ---

func TestDeriveProfileNameFromURL_MultiLabelHost(t *testing.T) {
	u, _ := url.Parse("https://work.example.com")
	got, err := deriveProfileNameFromURL(u)
	if err != nil {
		t.Fatalf("deriveProfileNameFromURL: %v", err)
	}
	if got != "work" {
		t.Errorf("got %q, want %q", got, "work")
	}
}

func TestDeriveProfileNameFromURL_SingleLabelHost(t *testing.T) {
	u, _ := url.Parse("https://localhost:8443")
	got, err := deriveProfileNameFromURL(u)
	if err != nil {
		t.Fatalf("deriveProfileNameFromURL: %v", err)
	}
	if got != "localhost" {
		t.Errorf("got %q, want %q", got, "localhost")
	}
}

func TestDeriveProfileNameFromURL_UppercaseHost_Lowercased(t *testing.T) {
	u, _ := url.Parse("https://WORK.EXAMPLE.COM")
	got, err := deriveProfileNameFromURL(u)
	if err != nil {
		t.Fatalf("deriveProfileNameFromURL: %v", err)
	}
	if got != "work" {
		t.Errorf("got %q, want %q", got, "work")
	}
}

func TestDeriveProfileNameFromURL_EmptyHost_Error(t *testing.T) {
	u, _ := url.Parse("https:///no-host")
	if _, err := deriveProfileNameFromURL(u); err == nil {
		t.Fatal("expected an error for an empty host")
	}
}

// --- fake daemon test harness ---

// fakeDeviceAuthServer captures the last POST /api/auth/device request body
// it saw and serves deviceAuthResponse as canonicalURL/deviceID/token, or a
// deviceAuthError-shaped 401 when wantCode is non-empty and does not match
// the redeemed code.
type fakeDeviceAuthServer struct {
	*httptest.Server
	lastRequest  deviceAuthRequest
	canonicalURL string
	deviceID     string
	token        string
	wantCode     string // "" = accept any code
	rejectDevice bool   // if true, DELETE /api/auth/devices/{id} returns 500
	sawDelete    bool
	deletePath   string
}

func newFakeDeviceAuthServer(t *testing.T) *fakeDeviceAuthServer {
	t.Helper()
	f := &fakeDeviceAuthServer{
		canonicalURL: "",
		deviceID:     "dev_fake123",
		token:        "tk_fake456",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/device", func(w http.ResponseWriter, r *http.Request) {
		var req deviceAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.lastRequest = req
		if f.wantCode != "" && req.Code != f.wantCode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid or expired pairing code"})
			return
		}
		canonical := f.canonicalURL
		if canonical == "" {
			canonical = f.Server.URL
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(deviceAuthResponse{
			DeviceID:     f.deviceID,
			Token:        f.token,
			CanonicalURL: canonical,
		})
	})
	mux.HandleFunc("/api/auth/devices/", func(w http.ResponseWriter, r *http.Request) {
		f.sawDelete = true
		f.deletePath = r.URL.Path
		if f.rejectDevice {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// --- login integration (TDD step 3) ---

func TestLogin_HappyPath_WritesTokenAndConfig(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	srv.canonicalURL = "https://canonical.example.com"
	withTestDefaultTransport(t, srv.Client().Transport)

	var stderr strings.Builder
	cmd := newLoginTestCmd(t, "work", "", "PAIR-CODE\n")
	cmd.SetErr(&stderr)

	if err := runLogin(cmd, []string{srv.Server.URL}); err != nil {
		t.Fatalf("runLogin: %v", err)
	}

	// Token file: canonical URL bound (decision 9), not the literal URL
	// the caller typed on the command line.
	tok, err := profiles.LoadToken("work")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if tok.DeviceID != "dev_fake123" || tok.Token != "tk_fake456" {
		t.Errorf("token = %+v", tok)
	}
	if tok.URL != "https://canonical.example.com" {
		t.Errorf("token.URL = %q, want the canonical_url from the response", tok.URL)
	}

	// config.yaml: profile entry also uses the canonical URL, and
	// default_profile got set since none existed before.
	cfgPath := filepath.Join(dir, "boid", "config.yaml")
	cfg, err := profiles.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Profiles["work"].URL != "https://canonical.example.com" {
		t.Errorf("profile url = %q", cfg.Profiles["work"].URL)
	}
	if cfg.DefaultProfile != "work" {
		t.Errorf("default_profile = %q, want %q", cfg.DefaultProfile, "work")
	}

	if !strings.Contains(stderr.String(), "logged in to work (https://canonical.example.com)") {
		t.Errorf("stderr missing success message: %q", stderr.String())
	}

	if srv.lastRequest.Code != "PAIR-CODE" {
		t.Errorf("server saw code %q, want %q", srv.lastRequest.Code, "PAIR-CODE")
	}
}

func TestLogin_DeviceNameDefaultsToHostname(t *testing.T) {
	withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	cmd := newLoginTestCmd(t, "work", "", "CODE\n")
	cmd.SetErr(&strings.Builder{})
	if err := runLogin(cmd, []string{srv.Server.URL}); err != nil {
		t.Fatalf("runLogin: %v", err)
	}

	hostname, herr := os.Hostname()
	if herr != nil {
		t.Skip("os.Hostname unavailable in this environment")
	}
	if srv.lastRequest.DeviceName != hostname {
		t.Errorf("device_name = %q, want hostname %q", srv.lastRequest.DeviceName, hostname)
	}
}

func TestLogin_DeviceNameFlagOverridesHostname(t *testing.T) {
	withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	cmd := newLoginTestCmd(t, "work", "my-laptop", "CODE\n")
	cmd.SetErr(&strings.Builder{})
	if err := runLogin(cmd, []string{srv.Server.URL}); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
	if srv.lastRequest.DeviceName != "my-laptop" {
		t.Errorf("device_name = %q, want %q", srv.lastRequest.DeviceName, "my-laptop")
	}
}

func TestLogin_WarnsOnOverwrite(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	configDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "profiles:\n  work:\n    url: https://old.example.com\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	var stderr strings.Builder
	cmd := newLoginTestCmd(t, "work", "", "CODE\n")
	cmd.SetErr(&stderr)
	if err := runLogin(cmd, []string{srv.Server.URL}); err != nil {
		t.Fatalf("runLogin: %v", err)
	}

	want := `warning: profile "work" already exists in config.yaml; overwriting`
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

func TestLogin_DoesNotOverrideExistingDefaultProfile(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	configDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "default_profile: home\nprofiles:\n  home:\n    url: unix:///run/user/1000/boid.sock\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	cmd := newLoginTestCmd(t, "work", "", "CODE\n")
	cmd.SetErr(&strings.Builder{})
	if err := runLogin(cmd, []string{srv.Server.URL}); err != nil {
		t.Fatalf("runLogin: %v", err)
	}

	cfg, err := profiles.LoadConfig(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultProfile != "home" {
		t.Errorf("default_profile = %q, want unchanged %q", cfg.DefaultProfile, "home")
	}
	if _, ok := cfg.Profiles["work"]; !ok {
		t.Error("expected the new 'work' profile to have been added")
	}
	if _, ok := cfg.Profiles["home"]; !ok {
		t.Error("expected the pre-existing 'home' profile to be preserved")
	}
}

func TestLogin_RejectsMalformedCanonicalURL(t *testing.T) {
	// The daemon returning an http:// canonical_url is the kind of
	// misbehaviour login must not persist to disk — that URL would end
	// up as the profile URL and the token file's URL, and every future
	// Bearer request would head somewhere unexpected. login should
	// reject up front rather than write it.
	withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	srv.canonicalURL = "http://plain.example.com"
	withTestDefaultTransport(t, srv.Client().Transport)

	cmd := newLoginTestCmd(t, "work", "", "CODE\n")
	cmd.SetErr(&strings.Builder{})
	err := runLogin(cmd, []string{srv.Server.URL})
	if err == nil {
		t.Fatal("expected an error for http:// canonical_url")
	}
	if _, terr := profiles.LoadToken("work"); terr == nil {
		t.Error("expected no token file to have been written after a rejected canonical_url")
	}
}

func TestLogin_WarnsOnOrphanTokenFile(t *testing.T) {
	// A token file left behind by a previous half-failed login (config
	// entry gone but token file still on disk) must still warn on
	// overwrite — silently blowing it away would lose the ability to
	// `logout` cleanly against whatever daemon that token was issued
	// against.
	withLoginXDGConfigHome(t)
	// Seed just the token file, NOT a config entry.
	if err := profiles.WriteToken("work", &profiles.Token{
		DeviceID: "dev_orphan",
		Token:    "tk_orphan",
		URL:      "https://old.example.com",
	}); err != nil {
		t.Fatalf("seed WriteToken: %v", err)
	}

	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	var stderr strings.Builder
	cmd := newLoginTestCmd(t, "work", "", "CODE\n")
	cmd.SetErr(&stderr)
	if err := runLogin(cmd, []string{srv.Server.URL}); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
	want := `warning: token file for profile "work" already exists`
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

func TestLogout_RevokesAgainstTokenURL_NotConfigURL(t *testing.T) {
	// A config file that points to origin B while the token was issued
	// against origin A must NOT cause the logout DELETE to be sent to
	// B (that would leak the Bearer token cross-origin). logout must
	// revoke against tok.URL, not prof.URL — see
	// revokeDeviceOnDaemon's doc comment for why.
	withLoginXDGConfigHome(t)

	// tokenDaemon = origin A: the token was issued here.
	tokenDaemon := newFakeDeviceAuthServer(t)
	// configDaemon = origin B: config.yaml points here.
	configDaemon := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, tokenDaemon.Client().Transport)

	// Seed a token that names origin A.
	if err := profiles.WriteToken("work", &profiles.Token{
		DeviceID: "dev_A",
		Token:    "tk_A",
		URL:      tokenDaemon.Server.URL,
	}); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	// Seed config that points to origin B.
	cfg := &profiles.Config{
		DefaultProfile: "work",
		Profiles:       map[string]profiles.Profile{"work": {URL: configDaemon.Server.URL}},
	}
	cfgPath, _ := profiles.ConfigPath()
	if err := profiles.WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&strings.Builder{})
	if err := runLogout(cmd, []string{"work"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	if !tokenDaemon.sawDelete {
		t.Error("token daemon (origin A) did NOT receive DELETE — revoke was misrouted")
	}
	if configDaemon.sawDelete {
		t.Error("config daemon (origin B) received DELETE — Bearer token was leaked cross-origin")
	}
}

func TestLogin_ServerRejectsCode_NoLocalStateWritten(t *testing.T) {
	withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	srv.wantCode = "REAL-CODE"
	withTestDefaultTransport(t, srv.Client().Transport)

	cmd := newLoginTestCmd(t, "work", "", "WRONG-CODE\n")
	cmd.SetErr(&strings.Builder{})
	err := runLogin(cmd, []string{srv.Server.URL})
	if err == nil {
		t.Fatal("expected an error for a rejected pairing code")
	}

	if _, err := profiles.LoadToken("work"); err == nil {
		t.Error("expected no token file to have been written after a failed login")
	}
}

func TestLogin_EmptyPairingCode_Error(t *testing.T) {
	withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	cmd := newLoginTestCmd(t, "work", "", "\n")
	cmd.SetErr(&strings.Builder{})
	if err := runLogin(cmd, []string{srv.Server.URL}); err == nil {
		t.Fatal("expected an error for an empty pairing code")
	}
}

func TestLogin_InvalidProfileSlug_Error(t *testing.T) {
	withLoginXDGConfigHome(t)
	cmd := newLoginTestCmd(t, "Not_Valid!", "", "CODE\n")
	if err := runLogin(cmd, []string{"https://work.example.com"}); err == nil {
		t.Fatal("expected an error for an invalid --profile slug")
	}
}

// --- logout integration (TDD step 4) ---

func TestLogout_HappyPath_RevokesAndCleansUpLocally(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)

	seedLoggedInProfile(t, dir, "work", srv.Server.URL, "dev_1", "tk_1")

	var stderr strings.Builder
	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&stderr)
	if err := runLogout(cmd, []string{"work"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	if !srv.sawDelete {
		t.Error("expected the daemon revoke DELETE to have been called")
	}
	if srv.deletePath != "/api/auth/devices/dev_1" {
		t.Errorf("delete path = %q, want %q", srv.deletePath, "/api/auth/devices/dev_1")
	}

	if _, err := profiles.LoadToken("work"); err == nil {
		t.Error("expected the token file to be removed")
	}
	cfg, err := profiles.LoadConfig(filepath.Join(dir, "boid", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Error("expected the config.yaml profile entry to be removed")
	}
	if !strings.Contains(stderr.String(), "logged out from work") {
		t.Errorf("stderr missing success message: %q", stderr.String())
	}
}

func TestLogout_DaemonUnreachable_WarnsButStillCleansUpLocally(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	// A closed server: connections fail immediately, simulating a daemon
	// that is unreachable.
	deadSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	withTestDefaultTransport(t, deadSrv.Client().Transport)
	deadSrv.Close()

	seedLoggedInProfile(t, dir, "work", deadURL, "dev_1", "tk_1")

	var stderr strings.Builder
	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&stderr)
	if err := runLogout(cmd, []string{"work"}); err != nil {
		t.Fatalf("runLogout must not hard-fail on a daemon revoke error: %v", err)
	}

	want := "warning: could not revoke device on daemon"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if !strings.Contains(stderr.String(), "token file will still be removed locally") {
		t.Errorf("stderr = %q, want the local-removal reassurance", stderr.String())
	}

	if _, err := profiles.LoadToken("work"); err == nil {
		t.Error("expected the token file to still be removed locally despite the revoke failure")
	}
	cfg, err := profiles.LoadConfig(filepath.Join(dir, "boid", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Error("expected the config.yaml profile entry to still be removed locally")
	}
}

func TestLogout_MissingToken_IdempotentCleanup(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	configDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "default_profile: work\nprofiles:\n  work:\n    url: https://work.example.com\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	// No token file written at all.

	var stderr strings.Builder
	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&stderr)
	if err := runLogout(cmd, []string{"work"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	if !strings.Contains(stderr.String(), "no token file for profile") {
		t.Errorf("stderr = %q, want a missing-token warning", stderr.String())
	}
	cfg, err := profiles.LoadConfig(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Error("expected the config.yaml profile entry to be removed")
	}
	if cfg.DefaultProfile != "" {
		t.Errorf("default_profile = %q, want unset", cfg.DefaultProfile)
	}
}

func TestLogout_MissingProfileConfig_OnlyDeletesTokenFile(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	// No config.yaml at all — just a lingering token file (e.g. the config
	// entry was hand-removed already).
	if err := profiles.WriteToken("ghost", &profiles.Token{DeviceID: "d", Token: "t", URL: "https://ghost.example.com"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	var stderr strings.Builder
	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&stderr)
	if err := runLogout(cmd, []string{"ghost"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	if !strings.Contains(stderr.String(), `profile "ghost" not found in config.yaml`) {
		t.Errorf("stderr = %q, want a missing-profile warning", stderr.String())
	}
	if _, err := profiles.LoadToken("ghost"); err == nil {
		t.Error("expected the token file to be removed")
	}
	// config.yaml must not have been conjured into existence for a profile
	// name that was never registered.
	if _, err := os.Stat(filepath.Join(dir, "boid", "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no config.yaml to be created, stat err = %v", err)
	}
}

func TestLogout_UnsetsMatchingDefaultProfile(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)
	seedLoggedInProfile(t, dir, "work", srv.Server.URL, "dev_1", "tk_1")

	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&strings.Builder{})
	if err := runLogout(cmd, []string{"work"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	cfg, err := profiles.LoadConfig(filepath.Join(dir, "boid", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultProfile != "" {
		t.Errorf("default_profile = %q, want unset after logging out the default profile", cfg.DefaultProfile)
	}
}

func TestLogout_KeepsDefaultProfileWhenDifferent(t *testing.T) {
	dir := withLoginXDGConfigHome(t)
	configDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newFakeDeviceAuthServer(t)
	withTestDefaultTransport(t, srv.Client().Transport)
	existing := "default_profile: home\nprofiles:\n" +
		"  home:\n    url: unix:///run/user/1000/boid.sock\n" +
		"  work:\n    url: " + srv.Server.URL + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := profiles.WriteToken("work", &profiles.Token{DeviceID: "dev_1", Token: "tk_1", URL: srv.Server.URL}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	cmd := newLogoutTestCmd(t)
	cmd.SetErr(&strings.Builder{})
	if err := runLogout(cmd, []string{"work"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}

	cfg, err := profiles.LoadConfig(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultProfile != "home" {
		t.Errorf("default_profile = %q, want unchanged %q", cfg.DefaultProfile, "home")
	}
	if _, ok := cfg.Profiles["work"]; ok {
		t.Error("expected 'work' to be removed")
	}
	if _, ok := cfg.Profiles["home"]; !ok {
		t.Error("expected 'home' to be preserved")
	}
}

func TestLogout_InvalidProfileSlug_Error(t *testing.T) {
	withLoginXDGConfigHome(t)
	cmd := newLogoutTestCmd(t)
	if err := runLogout(cmd, []string{"Not_Valid!"}); err == nil {
		t.Fatal("expected an error for an invalid profile slug")
	}
}

// seedLoggedInProfile writes both a config.yaml profile entry and a
// matching token file for name, as if a prior `boid login` had already
// succeeded against serverURL.
func seedLoggedInProfile(t *testing.T, xdgConfigDir, name, serverURL, deviceID, token string) {
	t.Helper()
	cfgPath := filepath.Join(xdgConfigDir, "boid", "config.yaml")
	cfg, err := profiles.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	newCfg := profiles.SetProfile(cfg, name, profiles.Profile{URL: serverURL})
	if err := profiles.WriteConfig(cfgPath, newCfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	tok := &profiles.Token{DeviceID: deviceID, Token: token, URL: serverURL}
	if err := profiles.WriteToken(name, tok); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
}

// --- command registration / annotations ---

func TestLoginLogoutCmd_Registration(t *testing.T) {
	names := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		names[c.Name()] = true
	}
	if !names["login"] {
		t.Error("rootCmd missing 'login' subcommand")
	}
	if !names["logout"] {
		t.Error("rootCmd missing 'logout' subcommand")
	}
}

func TestLoginCmd_Annotations(t *testing.T) {
	if loginCmd.Annotations[scopeAnnotationKey] != scopeNeutral {
		t.Errorf("loginCmd scope annotation = %q, want %q", loginCmd.Annotations[scopeAnnotationKey], scopeNeutral)
	}
	if loginCmd.Annotations[annotationSkipAutostart] != "skip" {
		t.Error("loginCmd must have annotationSkipAutostart=skip")
	}
}

func TestLogoutCmd_Annotations(t *testing.T) {
	if logoutCmd.Annotations[scopeAnnotationKey] != scopeNeutral {
		t.Errorf("logoutCmd scope annotation = %q, want %q", logoutCmd.Annotations[scopeAnnotationKey], scopeNeutral)
	}
	if logoutCmd.Annotations[annotationSkipAutostart] != "skip" {
		t.Error("logoutCmd must have annotationSkipAutostart=skip")
	}
}

func TestLoginCmd_RequiresExactlyOneArg(t *testing.T) {
	if err := loginCmd.Args(loginCmd, []string{}); err == nil {
		t.Error("expected an error for missing URL arg")
	}
	if err := loginCmd.Args(loginCmd, []string{"https://x"}); err != nil {
		t.Errorf("unexpected error for a single arg: %v", err)
	}
}

func TestLogoutCmd_RequiresExactlyOneArg(t *testing.T) {
	if err := logoutCmd.Args(logoutCmd, []string{}); err == nil {
		t.Error("expected an error for missing profile arg")
	}
	if err := logoutCmd.Args(logoutCmd, []string{"work"}); err != nil {
		t.Errorf("unexpected error for a single arg: %v", err)
	}
}

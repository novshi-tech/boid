package dispatcher

import (
	"os/exec"
	"strings"
	"testing"
)

func TestNormalizeOriginURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "https passthrough unchanged",
			raw:  "https://github.com/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "https passthrough without .git suffix stays as-is",
			raw:  "https://github.com/owner/repo",
			want: "https://github.com/owner/repo",
		},
		{
			name: "scp-like ssh github normalized to https",
			raw:  "git@github.com:owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "scp-like ssh bitbucket normalized to https",
			raw:  "git@bitbucket.org:owner/repo.git",
			want: "https://bitbucket.org/owner/repo.git",
		},
		{
			name: "ssh:// url normalized to https",
			raw:  "ssh://git@github.com/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "http url upgraded to https",
			raw:  "http://github.com/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "file url passthrough unchanged",
			raw:  "file:///tmp/some/repo",
			want: "file:///tmp/some/repo",
		},
		{
			name:    "empty url is an error",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "whitespace-only url is an error",
			raw:     "   ",
			wantErr: true,
		},
		{
			name:    "unrecognized form is an error",
			raw:     "not-a-url",
			wantErr: true,
		},
		{
			// MAJOR 5 (PR-2a codex round-1 review): a credential-bearing
			// https:// URL must be rejected outright, not stored — see
			// hasHTTPUserinfo's doc comment for the three surfaces that
			// would otherwise leak the embedded token.
			name:    "https url with embedded credentials is rejected",
			raw:     "https://user:pat@github.com/owner/repo.git",
			wantErr: true,
		},
		{
			name:    "https url with bare username (no password) is still rejected",
			raw:     "https://user@github.com/owner/repo.git",
			wantErr: true,
		},
		{
			name:    "http url with embedded credentials is rejected",
			raw:     "http://user:pat@github.com/owner/repo.git",
			wantErr: true,
		},
		{
			name: "git url normalized to https",
			raw:  "git://github.com/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "ssh url with explicit port drops the port on https rewrite",
			raw:  "ssh://git@github.com:2222/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			// BLOCKER 1 (PR-2a codex round-2 review): the pre-fix
			// hasHTTPUserinfo only ever inspected the authority component
			// (before the first "/"), so a query-embedded credential sailed
			// straight through NormalizeOriginURL unrejected — the URL then
			// got clone-attempted with the token in argv, and stored
			// verbatim into remote.origin.url / projects.upstream_url on
			// success.
			name:    "https url with credential-bearing query parameter is rejected",
			raw:     "https://github.com/owner/repo.git?access_token=SECRET",
			wantErr: true,
		},
		{
			name:    "https url with token query parameter is rejected",
			raw:     "https://github.com/owner/repo.git?token=SECRET",
			wantErr: true,
		},
		{
			// BLOCKER 1's "URL path" bypass: the "@" lands past the first
			// "/" after the host, exactly where hasHTTPUserinfo already
			// stopped looking (it cuts the authority off at the first "/").
			name:    "https url with credential-shaped pattern embedded in the path is rejected",
			raw:     "https://github.com/x-oauth-basic:SECRET@repo.git",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeOriginURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeOriginURL(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeOriginURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeOriginURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestNormalizeOriginURL_RejectedErrorsNeverLeakCredential pins BLOCKER 1's
// required test assertion (PR-2a codex round-2 review): every credential
// encoding form must be rejected at input (never returned as a normalized
// URL), AND the returned error's message itself must never contain the raw
// secret — a rejection whose own error text echoes the token back would
// defeat the entire point of rejecting it.
func TestNormalizeOriginURL_RejectedErrorsNeverLeakCredential(t *testing.T) {
	const secret = "s3cr3t-token-value"
	tests := []string{
		"https://user:" + secret + "@github.com/owner/repo.git",
		"https://" + secret + "@github.com/owner/repo.git",
		"https://github.com/owner/repo.git?access_token=" + secret,
		"https://github.com/owner/repo.git?token=" + secret,
		"https://github.com/owner/repo.git?password=" + secret,
		"https://github.com/x-oauth-basic:" + secret + "@repo.git",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := NormalizeOriginURL(raw)
			if err == nil {
				t.Fatalf("NormalizeOriginURL(%q) = %q, want error (credential must be rejected at input, never clone-attempted)", raw, got)
			}
			if got != "" {
				t.Errorf("NormalizeOriginURL(%q) returned a non-empty URL %q alongside an error", raw, got)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("NormalizeOriginURL(%q) error leaks the raw credential: %v", raw, err)
			}
		})
	}
}

// TestSanitizeURLForLogging pins the redaction helper every credential
// error-message/log site in this package routes through (PR-2a codex
// round-2 Blocker 1 sibling sweep).
func TestSanitizeURLForLogging(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "no credential shape, unchanged",
			raw:  "https://github.com/owner/repo.git",
			want: "https://github.com/owner/repo.git",
		},
		{
			name: "userinfo redacted",
			raw:  "https://user:pat@github.com/owner/repo.git",
			want: "https://REDACTED@github.com/owner/repo.git",
		},
		{
			name: "query credential redacted, key preserved",
			raw:  "https://github.com/owner/repo.git?access_token=SECRET",
			want: "https://github.com/owner/repo.git?access_token=REDACTED",
		},
		{
			name: "query credential redacted, second query param",
			raw:  "https://github.com/owner/repo.git?foo=bar&token=SECRET",
			want: "https://github.com/owner/repo.git?foo=bar&token=REDACTED",
		},
		{
			name: "path-embedded credential-shaped pattern redacted",
			raw:  "https://github.com/x-oauth-basic:SECRET@repo.git",
			want: "https://github.com/REDACTED@repo.git",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeURLForLogging(tt.raw); got != tt.want {
				t.Errorf("SanitizeURLForLogging(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCaptureUpstreamURL_NoGitRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := CaptureUpstreamURL(dir); err == nil {
		t.Fatal("expected error for a directory with no git repository")
	}
}

func TestCaptureUpstreamURL_NoOriginRemote(t *testing.T) {
	dir := t.TempDir()
	runGitForTest(t, dir, "init", "-q")
	if _, err := CaptureUpstreamURL(dir); err == nil {
		t.Fatal("expected error for a git repo with no origin remote")
	}
}

func TestCaptureUpstreamURL_SSHOriginNormalizedToHTTPS(t *testing.T) {
	dir := t.TempDir()
	runGitForTest(t, dir, "init", "-q")
	runGitForTest(t, dir, "remote", "add", "origin", "git@github.com:owner/repo.git")

	got, err := CaptureUpstreamURL(dir)
	if err != nil {
		t.Fatalf("CaptureUpstreamURL: %v", err)
	}
	if want := "https://github.com/owner/repo.git"; got != want {
		t.Errorf("CaptureUpstreamURL = %q, want %q", got, want)
	}
}

// runGitForTest runs `git <args...>` with cwd=dir, failing the test on error.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

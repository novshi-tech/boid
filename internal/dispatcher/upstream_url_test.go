package dispatcher

import (
	"os/exec"
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

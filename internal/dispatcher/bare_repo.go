package dispatcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/gitgateway"
)

// This file implements the daemon-side authenticated clone/fetch used by
// `boid project add <git-url>` / `boid project fetch <id>` (docs/plans/
// volume-only-daemon.md §論点b: "実装は既存の git gateway 経路を活用 (auth は
// 既存の gateway.forges 経由)"). Unlike the sandbox-side clone (which goes
// through gitgateway's HTTP reverse proxy at job-dispatch time, injecting
// credentials via a per-job registry token — see gitgateway_wire.go), this
// runs from the daemon process itself, so it talks to
// gitgateway.CredentialProvider directly: no job token, no reverse proxy,
// just "does this host have forge credentials configured, and if so what
// are they".
//
// Credentials are injected via a transient `-c http.extraHeader=...` git
// config override, never embedded in the clone/fetch URL argument — `git
// clone <url> <dest>` stores its URL argument verbatim into
// remote.origin.url, so a credentialed URL would leave the Basic-auth token
// sitting in plaintext inside the bare repo's own config file on disk. The
// header override is process-local and never touches dest's git config.

// CloneBareRepo clones rawURL into destPath as a `--mirror` bare repository
// (NOT plain `--bare`: a plain bare clone sets no fetch refspec on the
// `origin` remote at all, so a later `git fetch --all` populates FETCH_HEAD
// only and silently leaves every branch ref exactly as it was at clone
// time — useless as a daemon-managed fetch cache. `--mirror` configures
// `remote.origin.fetch = +refs/*:refs/*` (and mirror=true), which is what
// makes FetchBareRepo's `git fetch --all` actually update refs/heads/* in
// place; see docs/plans/volume-only-daemon.md §論点b's fetch 経路), injecting
// forge credentials from creds when rawURL's host is configured under
// gateway.forges. namespace scopes secret resolution the same way job
// dispatch does (the registering project's workspace slug —
// SecretNamespace's existing convention). A host creds has no opinion on
// (gitgateway.CredentialProvider.KnowsHost false, or creds itself nil)
// clones unauthenticated — correct for public repos and for SSH URLs, which
// this credential mechanism (HTTPS Basic auth) does not apply to at all.
func CloneBareRepo(ctx context.Context, rawURL, destPath string, creds *gitgateway.CredentialProvider, namespace string) error {
	extraArgs, err := credentialGitArgs(rawURL, creds, namespace)
	if err != nil {
		return err
	}
	args := append(append([]string{}, extraArgs...), "clone", "--mirror", "--", rawURL, destPath)
	return runGit(exec.CommandContext(ctx, "git", args...))
}

// FetchBareRepo runs `git fetch --all --prune` inside an existing
// daemon-managed bare (mirror) repo (`boid project fetch <id>`),
// re-resolving credentials against the repo's own remote.origin.url —
// which, per CloneBareRepo's doc comment, is always the plain
// (uncredentialed) URL the project was registered with.
func FetchBareRepo(ctx context.Context, bareRepoPath string, creds *gitgateway.CredentialProvider, namespace string) error {
	originURL, err := gitRemoteURL(ctx, bareRepoPath)
	if err != nil {
		return err
	}
	extraArgs, err := credentialGitArgs(originURL, creds, namespace)
	if err != nil {
		return err
	}
	args := append(append([]string{}, extraArgs...), "-C", bareRepoPath, "fetch", "--all", "--prune")
	return runGit(exec.CommandContext(ctx, "git", args...))
}

// gitRemoteURL returns bareRepoPath's `origin` remote URL.
func gitRemoteURL(ctx context.Context, bareRepoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", bareRepoPath, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: git remote get-url origin: %w", bareRepoPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// credentialGitArgs returns extra `-c ...` args to prepend to a git
// invocation, injecting forge Basic auth for rawURL's host when creds knows
// about it. Returns (nil, nil) — proceed unauthenticated — when creds is
// nil, the URL's host cannot be determined (e.g. a bare scp-like path with
// no recognizable host/path split), or the host has no gateway.forges
// entry; this mirrors gitgateway.CredentialProvider.KnowsHost's own
// fail-open convention for hosts the gateway has no opinion on (see that
// method's doc comment).
func credentialGitArgs(rawURL string, creds *gitgateway.CredentialProvider, namespace string) ([]string, error) {
	if creds == nil {
		return nil, nil
	}
	host, err := hostFromGitURL(rawURL)
	if err != nil || host == "" {
		return nil, nil
	}
	if !creds.KnowsHost(host) {
		return nil, nil
	}
	username, token, err := creds.Resolve(host, namespace)
	if err != nil {
		return nil, fmt.Errorf("resolve git credentials for %q: %w", host, err)
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+token))
	return []string{"-c", "http.extraHeader=" + header}, nil
}

// hostFromGitURL extracts just the host component from a git remote URL,
// reusing repoSlugFromOriginURL's (host_commands.go) existing form-parsing
// logic (https://, http://, ssh://, and the scp-like git@host:path form).
func hostFromGitURL(rawURL string) (string, error) {
	slug, err := repoSlugFromOriginURL(rawURL)
	if err != nil {
		return "", err
	}
	host, _, ok := strings.Cut(slug, "/")
	if !ok {
		return "", fmt.Errorf("unrecognized origin url form: %q", rawURL)
	}
	return host, nil
}

// runGit executes cmd, wrapping a failure with cmd's args and captured
// stderr for a useful daemon-log / API-error message.
func runGit(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, msg)
		}
		return fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	return nil
}

package dispatcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"regexp"
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
// (uncredentialed) URL the project was registered with. After a successful
// fetch, also refreshes bareRepoPath's symbolic HEAD to match the remote's
// current default branch (MAJOR 4, PR-2a codex round-1 review) — see
// refreshBareRepoHEAD's doc comment for why this needs its own step (a
// mirror clone's fetch refspec never touches HEAD on its own).
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
	if err := runGit(exec.CommandContext(ctx, "git", args...)); err != nil {
		return err
	}
	return refreshBareRepoHEAD(ctx, bareRepoPath, extraArgs)
}

// refreshBareRepoHEAD points bareRepoPath's symbolic HEAD at origin's
// current default branch (MAJOR 4, PR-2a codex round-1 review). A `--mirror`
// clone's fetch refspec (`+refs/*:refs/*`, see CloneBareRepo's doc comment)
// updates every refs/heads/* ref in place but never touches HEAD itself —
// so when a forge's default branch changes (e.g. main -> master), `boid
// project fetch` silently keeps reading project.yaml from the OLD branch
// forever; if that old branch is later deleted upstream (`--prune` removes
// the local ref too), the project permanently degrades ("HEAD: not a valid
// object name") even though the repository is otherwise perfectly healthy.
//
// `git remote set-head origin --auto` — the usual fix for this — does NOT
// work here: it resolves the remote's HEAD from the LOCAL
// refs/remotes/origin/* tracking namespace, which a --mirror clone's
// `+refs/*:refs/*` refspec never populates (refs land directly at
// refs/heads/*, with no "origin/" prefix at all — confirmed empirically:
// `git remote set-head origin --auto` against a --mirror clone whose remote
// renamed its default branch left the local HEAD pointing at the
// now-deleted old branch name). `git ls-remote --symref origin HEAD`
// instead asks the remote SERVER directly which branch its HEAD currently
// points at — no local tracking-ref dependency — and the result is applied
// locally via `git symbolic-ref`.
//
// Failure to determine or apply the new HEAD is intentionally non-fatal:
// the fetch itself already succeeded (every ref is up to date), so this is
// purely an improvement on top when it works, never a regression when it
// can't (e.g. a remote that reports no symref for HEAD at all, or a
// transient failure right after a successful fetch) — the project simply
// keeps reading from whatever HEAD already pointed at, exactly like before
// this refresh step existed.
func refreshBareRepoHEAD(ctx context.Context, bareRepoPath string, extraArgs []string) error {
	args := append(append([]string{}, extraArgs...), "-C", bareRepoPath, "ls-remote", "--symref", "origin", "HEAD")
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		// Best-effort: see the doc comment above for why a failure here
		// does not propagate.
		return nil
	}
	ref, err := parseSymrefHEAD(stdout.String())
	if err != nil {
		return nil
	}
	if err := runGit(exec.CommandContext(ctx, "git", "-C", bareRepoPath, "symbolic-ref", "HEAD", ref)); err != nil {
		// Best-effort (Minor 3, PR-2a codex round-2 review): this line used
		// to return the error directly, silently contradicting the doc
		// comment two paragraphs up — the fetch itself already succeeded
		// (every ref is up to date), so a failure applying the resolved
		// symref must not fail the whole `boid project fetch` any more than
		// the two ls-remote/parse failures above do.
		return nil
	}
	return nil
}

// parseSymrefHEAD extracts the target ref from `git ls-remote --symref
// <remote> HEAD`'s output, whose first matching line (when the remote
// advertises a symref for HEAD) looks like "ref: refs/heads/main\tHEAD".
func parseSymrefHEAD(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref: ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref: "))
		if len(fields) == 0 {
			continue
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("no symref reported for HEAD")
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
		return "", fmt.Errorf("unrecognized origin url form: %q", SanitizeURLForLogging(rawURL))
	}
	return host, nil
}

// credentialHeaderPattern matches an `Authorization: Basic <token>` (or
// `Bearer <token>`) value the way credentialGitArgs embeds it into a
// `-c http.extraHeader=...` argument, capturing everything up to (but not
// including) the token itself so redactGitArgs can splice a placeholder in
// its place without losing the surrounding `http.extraHeader=` prefix
// (useful for reading the error, still safe to log/return to a client).
var credentialHeaderPattern = regexp.MustCompile(`(?i)(Authorization:\s*(?:Basic|Bearer)\s+)\S+`)

// redactGitArgs returns a copy of args with any embedded credential value
// (the Basic/Bearer Authorization header credentialGitArgs injects via
// `-c http.extraHeader=...`) replaced with a redacted placeholder (PR-2a
// codex round-1 Blocker 1). Without this, runGit's error path below joined
// cmd.Args verbatim into the returned error — which CreateProjectFromGitURL
// / FetchProject then surface straight to the CLI/API caller and, for
// FetchProject, persist as a project's StatusMessage (readable via
// `boid project list`/`show` from then on) — so a single typo'd private-repo
// URL with a configured forge PAT would leak
// `http.extraHeader=Authorization: Basic <base64-user:token>` through both
// surfaces. Every arg is scanned (not just the known `-c` value) so this
// stays correct even if a future caller passes the header some other way.
// Every arg additionally passes through SanitizeURLForLogging (BLOCKER 1,
// PR-2a codex round-2 review sibling sweep): by construction, no caller of
// CloneBareRepo/FetchBareRepo should ever be able to reach this point with
// a credential-bearing URL argument any more (NormalizeOriginURL's
// validateURLForClone rejects it far earlier, before either function is
// ever invoked) — but this is exactly the same "costs nothing, closes the
// door defensively in case a future git version / caller ever changes
// that" reasoning the Basic/Bearer header redaction below already applies
// to stderr.
func redactGitArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		redacted := credentialHeaderPattern.ReplaceAllString(a, "${1}<redacted>")
		out[i] = SanitizeURLForLogging(redacted)
	}
	return out
}

// runGit executes cmd, wrapping a failure with cmd's args and captured
// stderr for a useful daemon-log / API-error message. Both the args and the
// captured stderr are passed through redactGitArgs first (Blocker 1 — see
// its doc comment): stderr is not known to ever echo the injected
// credential header back (git does not print the command line it was
// invoked with), but redacting it too costs nothing and closes that door
// defensively in case a future git version, `GIT_CURL_VERBOSE`, or a
// misbehaving credential helper ever changes that.
func runGit(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		argsStr := strings.Join(redactGitArgs(cmd.Args), " ")
		msg := strings.TrimSpace(redactGitArgs([]string{stderr.String()})[0])
		if msg != "" {
			return fmt.Errorf("%s: %w: %s", argsStr, err, msg)
		}
		return fmt.Errorf("%s: %w", argsStr, err)
	}
	return nil
}

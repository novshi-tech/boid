package dispatcher

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// repoSlugPlaceholder is the per-command env context variable expanded by
// ResolveHostCommands. See docs/plans/host-command-contract.md item 3: host
// commands run with a neutral cwd (no repo checkout), so repo context for
// tools like `gh` is instead injected via env at token-registration time.
const repoSlugPlaceholder = "${boid:repo_slug}"

// repoSlugFromOriginURL normalizes a git remote origin URL into a
// `host/owner/repo` slug. Supported forms:
//
//	https://github.com/owner/repo.git -> github.com/owner/repo
//	git@github.com:owner/repo.git     -> github.com/owner/repo
//	ssh://git@github.com/owner/repo.git -> github.com/owner/repo
//	ssh://git@github.com:2222/owner/repo.git -> github.com/owner/repo
//	git://github.com/owner/repo.git   -> github.com/owner/repo
//
// Non-github hosts are kept as-is. Returns an error if the URL cannot be
// parsed into a host/owner/repo shape.
// RepoSlugFromOriginURL is the exported form of repoSlugFromOriginURL: the
// same normalization gitgateway.NewRepoKey applies to a request path's
// host/owner/repo components, used here as the hash input for a
// URL-derived project id — see internal/api's
// deriveProjectIDFromURL. Exported specifically for that cross-package use;
// every existing in-package caller still calls the unexported form directly.
func RepoSlugFromOriginURL(url string) (string, error) {
	return repoSlugFromOriginURL(url)
}

func repoSlugFromOriginURL(url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", fmt.Errorf("empty origin url")
	}

	var hostAndPath string
	switch {
	case strings.HasPrefix(url, "https://"):
		hostAndPath = strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "http://"):
		hostAndPath = strings.TrimPrefix(url, "http://")
	// git:// carries no userinfo at all (it is the plain, unauthenticated
	// anonymous-fetch protocol), so there is nothing to strip — the whole
	// remainder is host/path, same shape as https:// / http://.
	case strings.HasPrefix(url, "git://"):
		hostAndPath = strings.TrimPrefix(url, "git://")
	case strings.HasPrefix(url, "ssh://"):
		rest := strings.TrimPrefix(url, "ssh://")
		if at := strings.Index(rest, "@"); at != -1 {
			rest = rest[at+1:]
		}
		// rest is now "host[:port]/owner/repo(.git)?". Split host from path
		// at the first '/', then drop an explicit port from the host — an
		// ssh port has nothing to do with the https port NormalizeOriginURL's
		// rewrite would otherwise carry over verbatim.
		host, path, ok := strings.Cut(rest, "/")
		if !ok {
			return "", fmt.Errorf("unrecognized origin url form: %q", url)
		}
		hostAndPath = stripSSHPort(host) + "/" + path
	default:
		// scp-like form: git@host:owner/repo.git
		at := strings.Index(url, "@")
		colon := strings.Index(url, ":")
		if at == -1 || colon == -1 || colon < at {
			return "", fmt.Errorf("unrecognized origin url form: %q", url)
		}
		host := url[at+1 : colon]
		path := url[colon+1:]
		hostAndPath = host + "/" + path
	}

	hostAndPath = strings.TrimSuffix(hostAndPath, ".git")
	hostAndPath = strings.TrimSuffix(hostAndPath, "/")
	if hostAndPath == "" || !strings.Contains(hostAndPath, "/") {
		return "", fmt.Errorf("unrecognized origin url form: %q", url)
	}
	return hostAndPath, nil
}

// stripSSHPort removes an explicit ":<port>" suffix from an ssh:// URL's
// host component, IPv6-safe: net.SplitHostPort correctly recognizes a
// bracketed IPv6 literal (a naive split at the first colon would mangle
// "[2001:db8::1]:2222"). Its result is re-bracketed here since the caller
// reassembles this into a "host/path" slug that NormalizeOriginURL later
// prefixes with "https://" — an IPv6 literal is only valid there in
// bracketed form.
func stripSSHPort(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		// No explicit ":<port>" suffix at all (the common case — a plain
		// hostname, or a bare IPv6 literal with no port) — nothing to strip.
		return host
	}
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// expandRepoSlugEnv rewrites occurrences of repoSlugPlaceholder inside env
// values with the derived repo slug. It mutates a fresh copy of env (never
// the caller's map) and only invokes getOriginURL/derives the slug lazily,
// the first time a placeholder is actually found — most commands have no
// ${boid:...} usage at all and should not pay for a git invocation.
//
// Unknown ${boid:...} placeholders are left untouched but logged, as a
// forward-compat signal for future context variables.
func expandRepoSlugEnv(name string, env map[string]string, projectDir string, getOriginURL func(string) (string, error)) map[string]string {
	if len(env) == 0 {
		return env
	}

	needsSlug := false
	for _, v := range env {
		if strings.Contains(v, repoSlugPlaceholder) {
			needsSlug = true
			break
		}
	}

	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	if !needsSlug {
		warnUnknownBoidVars(name, out)
		return out
	}

	slug := ""
	url, err := getOriginURL(projectDir)
	if err != nil {
		slog.Warn("host command env: could not derive ${boid:repo_slug}, expanding to empty string",
			"command", name, "reason", err)
	} else {
		slug, err = repoSlugFromOriginURL(url)
		if err != nil {
			slog.Warn("host command env: could not derive ${boid:repo_slug}, expanding to empty string",
				"command", name, "reason", err)
			slug = ""
		}
	}

	for k, v := range out {
		out[k] = strings.ReplaceAll(v, repoSlugPlaceholder, slug)
	}
	warnUnknownBoidVars(name, out)
	return out
}

// warnUnknownBoidVars logs a warning for any remaining `${boid:...}` token
// other than repo_slug (which is expanded before this runs). It is a
// forward-compat signal: unrecognized context variables are left in the
// value untouched rather than failing the dispatch.
func warnUnknownBoidVars(name string, env map[string]string) {
	for k, v := range env {
		start := 0
		for {
			idx := strings.Index(v[start:], "${boid:")
			if idx == -1 {
				break
			}
			idx += start
			end := strings.Index(v[idx:], "}")
			if end == -1 {
				break
			}
			token := v[idx : idx+end+1]
			start = idx + end + 1
			if token == repoSlugPlaceholder {
				continue
			}
			slog.Warn("host command env: unrecognized ${boid:...} context variable left untouched",
				"command", name, "env_key", k, "token", token)
		}
	}
}

func ResolveHostCommands(
	builtins []string,
	hostCommands map[string]orchestrator.CommandDef,
	projectDir string,
	lookPath func(string) (string, error),
	getOriginURL func(string) (string, error),
) (byPath map[string]orchestrator.CommandDef, byName map[string]orchestrator.CommandDef, err error) {
	out := make(map[string]orchestrator.CommandDef)

	for _, name := range builtins {
		if name == "boid" || name == "git" || name == "fetch" {
			continue
		}
		if _, ok := hostCommands[name]; ok {
			continue
		}
		absPath, lookErr := lookPath(name)
		if lookErr != nil {
			return nil, nil, fmt.Errorf("host command %q not found on host: %w", name, lookErr)
		}
		if _, dup := out[absPath]; dup {
			continue
		}
		out[absPath] = orchestrator.CommandDef{Name: name, Path: absPath}
	}

	for name, def := range hostCommands {
		if name == "boid" || name == "git" || name == "fetch" {
			continue
		}
		var absPath string
		if def.Path != "" {
			p := def.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(projectDir, p)
			}
			if _, statErr := os.Stat(p); statErr != nil {
				return nil, nil, fmt.Errorf("host_commands.%s.path %q does not exist on host", name, def.Path)
			}
			absPath = p
		} else {
			p, lookErr := lookPath(name)
			if lookErr != nil {
				return nil, nil, fmt.Errorf("host command %q not found on host: %w", name, lookErr)
			}
			absPath = p
		}
		if _, dup := out[absPath]; dup {
			continue
		}
		cd := def
		cd.Name = name
		cd.Path = absPath
		cd.Env = expandRepoSlugEnv(name, def.Env, projectDir, getOriginURL)
		out[absPath] = cd
	}

	byName = make(map[string]orchestrator.CommandDef, len(out))
	for _, def := range out {
		byName[def.Name] = def
	}

	return out, byName, nil
}

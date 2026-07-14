package dispatcher

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// repoSlugPlaceholder is the per-command env context variable expanded by
// ResolveHostCommands. See docs/plans/host-command-contract.md item 3: host
// commands run with a neutral cwd (no repo checkout), so repo context for
// tools like `gh` is instead injected via env at token-registration time.
const repoSlugPlaceholder = "${boid:repo_slug}"

// GitOriginURL returns the `git config --get remote.origin.url` value for
// dir, or an error if git is missing, dir is not a repo, or no origin is
// configured. It deliberately uses cmd.Dir rather than `git -C dir`: this
// repo's sandbox git wrapper rejects `-C`, and cmd.Dir works everywhere
// (production and sandboxed callers alike).
//
// This is the production getOriginURL implementation passed to
// ResolveHostCommands; it is exported so callers outside this package
// (internal/server/api_store.go) can pass it too.
func GitOriginURL(dir string) (string, error) {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git config --get remote.origin.url: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// repoSlugFromOriginURL normalizes a git remote origin URL into a
// `host/owner/repo` slug. Supported forms:
//
//	https://github.com/owner/repo.git -> github.com/owner/repo
//	git@github.com:owner/repo.git     -> github.com/owner/repo
//	ssh://git@github.com/owner/repo.git -> github.com/owner/repo
//
// Non-github hosts are kept as-is. Returns an error if the URL cannot be
// parsed into a host/owner/repo shape.
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
	case strings.HasPrefix(url, "ssh://"):
		rest := strings.TrimPrefix(url, "ssh://")
		if at := strings.Index(rest, "@"); at != -1 {
			rest = rest[at+1:]
		}
		hostAndPath = rest
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

// ResolveHostCommands turns the orchestrator-side host command map (keyed by
// the user-declared name) into a map keyed by the absolute path that the
// boid shim will be bind-mounted at inside the sandbox. The absolute path is
// also written back into each entry's Path so the broker spawns the right
// binary on the host without a second lookup.
//
// The same map is used both as the broker's policy table and as the source
// of shim mount targets; sharing a single resolved view guarantees that the
// `os.Executable()` value the shim sends will match a key the broker holds.
//
// The names "boid", "git", and "fetch" are excluded, each for a different
// reason: "boid" has a dedicated bind mount + builtin policy elsewhere;
// "fetch" is a broker builtin (`FetchRequest`) without a host binary at all;
// "git" is neither a broker builtin nor a shim — it's a real binary reached
// via the base rbind of /usr, but the name is reserved here so a user
// `host_commands.git:` entry doesn't try to overlay a shim onto that path
// and break the sandbox-side git that the git gateway clone flow depends on.
//
// `projectDir` is used to resolve relative paths declared in
// host_commands.<name>.path, and as the working directory for the origin URL
// lookup that expands `${boid:repo_slug}` in Env values (see
// docs/plans/host-command-contract.md item 3). `lookPath` and `getOriginURL`
// are parameterized for tests; production callers pass exec.LookPath and
// GitOriginURL. There are only two production call sites (runner.go,
// api_store.go), which is few enough that threading a parameter through them
// (matching the existing lookPath convention) is simpler than a
// package-level var seam.
func ResolveHostCommands(
	builtins []string,
	hostCommands map[string]orchestrator.CommandDef,
	projectDir string,
	lookPath func(string) (string, error),
	getOriginURL func(string) (string, error),
) (map[string]orchestrator.CommandDef, error) {
	out := make(map[string]orchestrator.CommandDef)

	for _, name := range builtins {
		if name == "boid" || name == "git" || name == "fetch" {
			continue
		}
		if _, ok := hostCommands[name]; ok {
			continue
		}
		absPath, err := lookPath(name)
		if err != nil {
			return nil, fmt.Errorf("host command %q not found on host: %w", name, err)
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
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("host_commands.%s.path %q does not exist on host", name, def.Path)
			}
			absPath = p
		} else {
			p, err := lookPath(name)
			if err != nil {
				return nil, fmt.Errorf("host command %q not found on host: %w", name, err)
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

	return out, nil
}

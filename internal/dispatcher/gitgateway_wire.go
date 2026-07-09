package dispatcher

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// registerGatewayToken registers this job's git gateway job token (self
// project fetch or fetch+push per Visibility.Writable, workspace peers and
// workspace extra_repos fetch-only) and returns the gateway's sandbox-facing
// base URL alongside the token, tracking the token so UnregisterJob can
// revoke it when the job completes.
//
// Both return values are empty when the gateway isn't wired (r.GitGateway ==
// nil). PR4 is inert (docs/plans/git-gateway-cutover.md): SandboxRuntimeInfo
// carries GatewayURL/GatewayJobToken but nothing inside the sandbox consumes
// them yet — the runner clone sequence is PR5, the env var advertise is PR6.
func (r *Runner) registerGatewayToken(jobID string, spec *orchestrator.JobSpec, workspaceID string) (gatewayURL, token string) {
	if r.GitGateway == nil {
		return "", ""
	}
	repos := r.buildGatewayRepos(spec, workspaceID)
	token = r.GitGateway.Register(repos)

	r.gatewayMu.Lock()
	if r.gatewayTokens == nil {
		r.gatewayTokens = make(map[string]string)
	}
	r.gatewayTokens[jobID] = token
	r.gatewayMu.Unlock()

	if r.GatewayURL != nil {
		gatewayURL = *r.GatewayURL
	}
	return gatewayURL, token
}

// buildGatewayRepos builds the job-token-scoped repo permission set for the
// git gateway registry:
//
//   - self project: PermFetchPush when spec.Visibility.Writable (task.readonly
//     / command.readonly determined this upstream — dispatcher only reads the
//     already-resolved flag, docs/plans/git-gateway-cutover.md 「readonly の
//     意味論変更: FS-RO → transport-RO」), PermFetch otherwise.
//   - workspace peers: every other project sharing workspaceID, PermFetch
//     only (fetch-only — writing to a peer means a cross-project child task
//     instead, per the workspace peer design).
//   - workspace extra_repos: the read-only allowlist declared in
//     workspace.yaml (WorkspaceMeta.ExtraRepos), PermFetch only.
//
// Projects/peers/extra_repos entries without a resolvable upstream_url (or
// whose upstream_url doesn't parse into host/owner/repo) are skipped with a
// warning rather than erroring — PR4 is inert (nobody consumes the
// registration yet) and requiring upstream_url here would be premature: that
// requirement is deferred to cutover (PR6), per
// orchestrator.RequireUpstreamURL's own doc comment.
func (r *Runner) buildGatewayRepos(spec *orchestrator.JobSpec, workspaceID string) map[gitgateway.RepoKey]gitgateway.Permission {
	if r.Projects == nil || spec == nil {
		return nil
	}
	repos := make(map[gitgateway.RepoKey]gitgateway.Permission)

	if self, err := r.Projects.GetProject(spec.ProjectID); err == nil && self != nil && self.UpstreamURL != "" {
		key, err := repoKeyFromUpstreamURL(self.UpstreamURL)
		if err != nil {
			slog.Warn("git gateway: could not parse project upstream_url",
				"project_id", spec.ProjectID, "upstream_url", self.UpstreamURL, "error", err)
		} else {
			perm := gitgateway.PermFetch
			if spec.Visibility.Writable {
				perm = gitgateway.PermFetchPush
			}
			repos[key] = perm
		}
	}

	if workspaceID == "" {
		return repos
	}

	if projects, err := r.Projects.ListProjects(); err == nil {
		for _, p := range projects {
			if p == nil || p.ID == "" || p.ID == spec.ProjectID || p.WorkspaceID != workspaceID || p.UpstreamURL == "" {
				continue
			}
			key, err := repoKeyFromUpstreamURL(p.UpstreamURL)
			if err != nil {
				slog.Warn("git gateway: could not parse peer project upstream_url",
					"project_id", p.ID, "upstream_url", p.UpstreamURL, "error", err)
				continue
			}
			if _, exists := repos[key]; !exists {
				repos[key] = gitgateway.PermFetch
			}
		}
	}

	if r.Workspaces != nil {
		if wsMeta, err := r.Workspaces.Load(workspaceID); err == nil && wsMeta != nil {
			for _, url := range wsMeta.ExtraRepos {
				key, err := repoKeyFromUpstreamURL(url)
				if err != nil {
					slog.Warn("git gateway: could not parse workspace extra_repos entry",
						"workspace_id", workspaceID, "url", url, "error", err)
					continue
				}
				if _, exists := repos[key]; !exists {
					repos[key] = gitgateway.PermFetch
				}
			}
		}
	}

	return repos
}

// repoKeyFromUpstreamURL parses a captured upstream_url (or a workspace
// extra_repos entry, in any form repoSlugFromOriginURL accepts — HTTPS or
// SSH) into a gitgateway.RepoKey. It always routes through
// gitgateway.NewRepoKey so the register-side and lookup-side (gitgateway's
// parsePath -> route.repoKey()) normalization stay in lockstep (".git" suffix
// stripping in particular — see .claude/skills/boid-review/references/wiring-seams.md
// 「gitgateway RepoKey normalization」).
//
// GitHub and Bitbucket Cloud URLs always resolve to exactly host/owner/repo;
// anything else (e.g. a nested GitLab subgroup) is out of scope for the
// gateway's route pattern and returns an error rather than silently
// mis-keying the repo.
func repoKeyFromUpstreamURL(upstreamURL string) (gitgateway.RepoKey, error) {
	slug, err := repoSlugFromOriginURL(upstreamURL)
	if err != nil {
		return "", err
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 3 {
		return "", fmt.Errorf("upstream_url %q does not resolve to host/owner/repo (got %d path segments)", upstreamURL, len(parts))
	}
	return gitgateway.NewRepoKey(parts[0], parts[1], parts[2]), nil
}

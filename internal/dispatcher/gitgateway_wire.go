package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// registerGatewayToken registers this job's git gateway job token (self
// project fetch or fetch+push per Visibility.Writable, workspace peers and
// workspace extra_repos fetch-only), scoped to spec.SecretNamespace, and
// returns the gateway's sandbox-facing base URL alongside the token,
// tracking the token so UnregisterJob can revoke it when the job completes.
//
// Both return values are empty when the gateway isn't wired (r.GitGateway ==
// nil).
func (r *Runner) registerGatewayToken(jobID string, spec *orchestrator.JobSpec, workspaceID string) (gatewayURL, token string) {
	if r.GitGateway == nil {
		return "", ""
	}
	repos := r.buildGatewayRepos(spec, workspaceID)
	token = r.GitGateway.Register(repos, spec.SecretNamespace)
	r.GitGateway.ObservePush(token, func(repo gitgateway.RepoKey, ref string) {
		message := fmt.Sprintf("git push requested: job=%s repo=%s ref=%s base_branch=%s", jobID, repo, ref, spec.Env["BOID_BASE_BRANCH"])
		slog.Info(message)
		if spec.TaskID == "" || r.DB == nil {
			return
		}
		payload, _ := json.Marshal(map[string]string{"message": message, "job_id": jobID, "repo": string(repo), "ref": ref, "base_branch": spec.Env["BOID_BASE_BRANCH"]})
		if err := orchestrator.CreateAction(context.Background(), r.DB, &orchestrator.Action{TaskID: spec.TaskID, Type: "progress", Payload: payload, Actor: orchestrator.ActorDaemon}, nil); err != nil {
			slog.Warn("record git push diagnostic", "job_id", jobID, "error", err)
		}
	})

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
//     already-resolved flag), PermFetch otherwise.
//   - workspace peers: every other project sharing workspaceID, PermFetch
//     only (writing to a peer means a cross-project child task instead).
//   - workspace extra_repos: the read-only allowlist declared in
//     workspace.yaml (WorkspaceMeta.ExtraRepos), PermFetch only.
//
// Projects/peers/extra_repos entries without a resolvable upstream_url (or
// whose upstream_url doesn't parse into host/owner/repo) are skipped with a
// warning rather than erroring (see orchestrator.RequireUpstreamURL's own
// doc comment for where upstream_url becomes required).
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

// buildPeerAdvertise advertises gateway URLs and the directories boid checkout creates.
func (r *Runner) buildPeerAdvertise(workspacePeers map[string]string, gatewayURL, gatewayToken string) map[string]PeerAdvertise {
	if len(workspacePeers) == 0 || gatewayURL == "" || gatewayToken == "" || r.Projects == nil {
		return nil
	}
	out := make(map[string]PeerAdvertise, len(workspacePeers))
	for peerID := range workspacePeers {
		proj, err := r.Projects.GetProject(peerID)
		if err != nil || proj == nil || proj.UpstreamURL == "" {
			continue
		}
		key, err := repoKeyFromUpstreamURL(proj.UpstreamURL)
		if err != nil {
			slog.Warn("git gateway: cannot build peer advertise, upstream_url did not parse",
				"peer_project_id", peerID, "upstream_url", proj.UpstreamURL, "error", err)
			continue
		}
		name := string(key)
		if parts := strings.Split(name, "/"); len(parts) == 3 {
			name = parts[2]
		}
		out[peerID] = PeerAdvertise{
			Name:     name,
			CloneURL: gatewayURL + gitgateway.PathPrefix + gatewayToken + "/" + string(key) + ".git",
			CloneDir: sandboxCloneDir(name),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// repoKeyFromUpstreamURL parses a captured upstream_url (or a workspace
// extra_repos entry, in any form repoSlugFromOriginURL accepts — HTTPS or
// SSH) into a gitgateway.RepoKey. It always routes through
// gitgateway.NewRepoKey so the register-side and lookup-side (gitgateway's
// parsePath -> route.repoKey()) normalization stay in lockstep — see
// wiring-seams.md "gitgateway RepoKey normalization".
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

// checkoutInputs resolves checkout identity and the task or workspace fork point for every job kind.
func (r *Runner) checkoutInputs(spec *orchestrator.JobSpec, workspaceID string) (primaryRepo, forkPoint string) {
	if r.Projects != nil {
		if project, err := r.Projects.GetProject(spec.ProjectID); err == nil && project != nil && project.UpstreamURL != "" {
			if key, err := repoKeyFromUpstreamURL(project.UpstreamURL); err == nil {
				primaryRepo = string(key)
			}
		}
	}
	forkPoint = spec.Env["BOID_FORK_POINT"]
	if forkPoint == "" && r.Hydrator != nil && spec.ProjectID != "" {
		if meta, err := r.Hydrator.GetWithWorkspace(context.Background(), spec.ProjectID); err == nil && meta != nil {
			forkPoint = meta.ForkPoint
		}
	}
	if forkPoint == "" && r.Workspaces != nil {
		slug, err := normalizeWorkspaceSlug(workspaceID)
		if err != nil {
			return primaryRepo, forkPoint
		}
		if meta, err := r.Workspaces.Load(slug); err == nil && meta != nil {
			forkPoint = meta.ForkPoint
		}
	}
	return primaryRepo, forkPoint
}

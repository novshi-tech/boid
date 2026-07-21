package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// registerGatewayToken registers this job's git gateway job token (self
// project fetch or fetch+push per Visibility.Writable, workspace peers and
// workspace extra_repos fetch-only), scoped to spec.SecretNamespace (post-
// cutover 改善 §1 workspace-scoped PAT namespace — SecretNamespace is
// already hydrated to the workspace ID by orchestrator.ProjectStore
// .GetWithWorkspace by the time a JobSpec reaches Dispatch), and returns the
// gateway's sandbox-facing base URL alongside the token, tracking the token
// so UnregisterJob can revoke it when the job completes.
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
	token = r.GitGateway.Register(repos, spec.SecretNamespace)

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

// buildGatewayCloneURL builds the full gateway clone URL for spec's own
// project — "<gatewayURL>/j/<gatewayToken>/<host>/<owner>/<repo>.git" —
// which the opt-in sandbox-clone path (docs/plans/git-gateway-cutover.md
// PR5) threads through SandboxRuntimeInfo.GatewayCloneURL for the runner to
// `git clone`. Returns "" (and logs a warning on a resolution failure) when
// any input is missing: gatewayURL/gatewayToken empty (gateway unwired),
// Projects unset, or the project's own upstream_url is empty/unparseable.
// Callers only invoke this when spec.Visibility.Clone != nil, so the lookup
// never runs for the default (non-opt-in) dispatch path.
func (r *Runner) buildGatewayCloneURL(spec *orchestrator.JobSpec, gatewayURL, gatewayToken string) string {
	if spec == nil || gatewayURL == "" || gatewayToken == "" || r.Projects == nil {
		return ""
	}
	self, err := r.Projects.GetProject(spec.ProjectID)
	if err != nil || self == nil || self.UpstreamURL == "" {
		slog.Warn("git gateway: cannot build clone URL, project has no captured upstream_url",
			"project_id", spec.ProjectID)
		return ""
	}
	key, err := repoKeyFromUpstreamURL(self.UpstreamURL)
	if err != nil {
		slog.Warn("git gateway: cannot build clone URL, upstream_url did not parse",
			"project_id", spec.ProjectID, "upstream_url", self.UpstreamURL, "error", err)
		return ""
	}
	// gatewayURL (Server.GatewayURL(), e.g. "http://10.0.2.2:<port>") never
	// has a trailing slash; gitgateway.PathPrefix ("/j/") already supplies
	// the leading one, matching the route gitgateway.parsePath expects.
	return gatewayURL + gitgateway.PathPrefix + gatewayToken + "/" + string(key) + ".git"
}

// buildPeerAdvertise resolves the {name, clone URL, reference path, clone
// dir} view of workspacePeers (docs/plans/git-gateway-cutover.md PR6 cutover
// 「5. peer advertise の変更」 — replaces the pre-cutover host path
// enumeration; CloneDir added by the workspace 親化リファクタリング, nose
// 2026-07-13 decision). Feeds SandboxRuntimeInfo.WorkspacePeerAdvertise,
// currently unread by BuildSandboxSpec — the environment.yaml
// `workspace_projects` section this used to feed was removed by the
// environment.yaml 縮退 (docs/plans/phase5-shim-and-task-context.md 決定事項
// 4, Phase 5b PR5); see that field's own doc comment. Returns nil when the
// gateway isn't wired (gatewayURL/
// gatewayToken empty) or Projects is unset; an individual peer with no
// resolvable upstream_url is skipped (with a warning) rather than aborting
// the whole map — same fail-soft posture as buildGatewayRepos.
//
// CloneDir's meta.name resolution (post-cutover 改善候補 §4 残タスク 1,
// docs/plans/git-gateway-cutover.md): r.Projects (orchestrator.
// DBProjectCatalog) is a bare `SELECT ... FROM projects` that never reads
// project.yaml, so proj.Meta is always the zero value — resolving the name
// from proj alone would always degrade to projectDirName's
// filepath.Base(WorkDir) fallback. When r.Hydrator is set, it is consulted
// instead (GetWithWorkspace parses project.yaml and merges workspace.yaml),
// giving peers the same meta.name support the self project already has via
// Visibility.ProjectName (cloneDirNameForVisibility). r.Hydrator == nil, or
// a hydration error for a given peer, fails soft to the basename fallback
// with a warning rather than skipping the peer or the whole map — advertise
// is best-effort and the job must still be able to dispatch on the self
// project alone.
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
		metaName := proj.Meta.Name
		if r.Hydrator != nil {
			if hydrated, hErr := r.Hydrator.GetWithWorkspace(context.Background(), peerID); hErr != nil {
				slog.Warn("git gateway: peer meta hydration failed, falling back to basename for clone dir",
					"peer_project_id", peerID, "error", hErr)
			} else if hydrated != nil {
				metaName = hydrated.Name
			}
		}
		out[peerID] = PeerAdvertise{
			Name:          name,
			CloneURL:      gatewayURL + gitgateway.PathPrefix + gatewayToken + "/" + string(key) + ".git",
			ReferencePath: fmt.Sprintf(sandboxClonePeerReferenceDirFmt, peerID),
			CloneDir:      sandboxCloneDir(projectDirName(metaName, proj.WorkDir)),
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

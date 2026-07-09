package gitgateway

import (
	"fmt"
	"regexp"
	"strings"
)

// Smart HTTP endpoint names (git-http-backend(1)). These are the only three
// endpoints the gateway forwards — docs/plans/git-gateway-cutover.md PR3:
// "git smart HTTP は 2 endpoint のみ".
const (
	EndpointInfoRefs    = "info/refs"
	EndpointUploadPack  = "git-upload-pack"
	EndpointReceivePack = "git-receive-pack"
)

// PathPrefix is the fixed prefix every gateway route starts with:
// /j/<job-token>/<host>/<owner>/<repo>[.git]/<endpoint>.
const PathPrefix = "/j/"

// Operation is the git operation implied by a request, derived from the
// endpoint name (and, for info/refs, the ?service= query parameter) rather
// than the HTTP method — docs/plans/git-gateway-cutover.md PR3: "push は
// git-receive-pack、fetch は git-upload-pack で判別 (endpoint 名 + service
// query param 両方)".
type Operation string

const (
	OpFetch Operation = "fetch" // git-upload-pack
	OpPush  Operation = "push"  // git-receive-pack
)

// RepoKey identifies a repo by its normalized "<host>/<owner>/<repo>" form —
// always suffix-free. docs/plans/git-gateway-cutover.md PR3 節の設計調整:
// PR2 の Opus レビューで NormalizeOriginURL の HTTPS/SSH 非対称
// (HTTPS 入力は suffix なし passthrough、SSH 入力は suffix 付与) が浮上した。
// gateway はこの非対称を吸収する層として、".git" suffix の有無を問わず
// 同じ RepoKey に正規化する — 登録時 (Registry.Register) も lookup 時
// (parsePath 経由) も必ずこの型を経由させ、吸収ロジックを一箇所に閉じる。
type RepoKey string

// NewRepoKey builds a normalized RepoKey from path components, stripping a
// ".git" suffix from repo if present.
func NewRepoKey(host, owner, repo string) RepoKey {
	repo = strings.TrimSuffix(repo, ".git")
	return RepoKey(host + "/" + owner + "/" + repo)
}

// pathPattern matches /j/<token>/<host>/<owner>/<repo>[.git]/<endpoint>.
// The repo group is non-greedy so an optional ".git" suffix is consumed by
// the following non-capturing group rather than becoming part of the repo
// name; both "<repo>.git/<endpoint>" and "<repo>/<endpoint>" match with the
// same captured repo name.
var pathPattern = regexp.MustCompile(`^/j/([^/]+)/([^/]+)/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

// route is the parsed shape of a gateway request path.
type route struct {
	token    string
	host     string
	owner    string
	repo     string // suffix-free
	endpoint string
}

// parsePath parses a request path of the form
// /j/<token>/<host>/<owner>/<repo>[.git]/<endpoint>. It returns an error for
// any path that doesn't match this exact shape (unrecognized routes are
// treated as 404s by the caller, not 401/403 — those statuses are reserved
// for token/authorization failures on well-formed routes).
func parsePath(path string) (route, error) {
	m := pathPattern.FindStringSubmatch(path)
	if m == nil {
		return route{}, fmt.Errorf("gitgateway: path %q does not match %s<token>/<host>/<owner>/<repo>[.git]/<endpoint>", path, PathPrefix)
	}
	return route{
		token:    m[1],
		host:     m[2],
		owner:    m[3],
		repo:     m[4],
		endpoint: m[5],
	}, nil
}

// repoKey returns the normalized RepoKey for the route.
func (r route) repoKey() RepoKey {
	return NewRepoKey(r.host, r.owner, r.repo)
}

// upstreamPath returns the canonical (".git"-suffixed) upstream request path
// for the route, e.g. "/owner/repo.git/info/refs".
func (r route) upstreamPath() string {
	return "/" + r.owner + "/" + r.repo + ".git/" + r.endpoint
}

// methodForEndpoint returns the only HTTP method a given smart-HTTP endpoint
// accepts: GET for info/refs, POST for the two service endpoints.
func methodForEndpoint(endpoint string) string {
	if endpoint == EndpointInfoRefs {
		return "GET"
	}
	return "POST"
}

// operationForEndpoint derives the Operation (fetch or push) implied by a
// request. For git-upload-pack / git-receive-pack the endpoint name alone is
// definitive. For info/refs the ?service= query parameter distinguishes a
// fetch-side ref advertisement from a push-side one; a missing or
// unrecognized service value is an error (git's dumb HTTP protocol, which
// omits ?service=, is out of scope — the gateway only speaks smart HTTP).
func operationForEndpoint(endpoint, service string) (Operation, error) {
	switch endpoint {
	case EndpointUploadPack:
		return OpFetch, nil
	case EndpointReceivePack:
		return OpPush, nil
	case EndpointInfoRefs:
		switch service {
		case "git-upload-pack":
			return OpFetch, nil
		case "git-receive-pack":
			return OpPush, nil
		default:
			return "", fmt.Errorf("gitgateway: info/refs request missing or has unrecognized ?service= parameter: %q", service)
		}
	default:
		return "", fmt.Errorf("gitgateway: unrecognized endpoint %q", endpoint)
	}
}

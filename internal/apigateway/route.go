package apigateway

import (
	"fmt"
	"path"
	"strings"
)

// PathPrefix is the fixed prefix every gateway route starts with:
// /api/<job-token>/<service>/<path...> — docs/plans/api-gateway.md §1
// ("ルーティングと sandbox からの見え方"). Mirrors gitgateway.PathPrefix's role
// for the sibling git gateway, using a distinct prefix so both gateways can
// share a single listener, dispatching on this prefix
// (docs/plans/api-gateway.md 論点1: "同居 (path prefix /j/ と /api/ で分岐)").
const PathPrefix = "/api/"

// route is the parsed shape of a gateway request path.
type route struct {
	token   string
	service string
	// path is the cleaned, always-absolute upstream path tail (e.g. "/" or
	// "/v1/users") — guaranteed by parsePath to never contain a ".."
	// segment, so no caller can walk it above the service's own base_url
	// root (docs/plans/api-gateway.md §5 「path 正規化」).
	path string
}

// parsePath parses a request path of the form
// /api/<token>/<service>/<path...>. It returns an error for any path that
// doesn't match this exact shape (unrecognized routes are treated as 404s by
// the caller, not 401/403 — those statuses are reserved for token/
// authorization failures on well-formed routes, mirroring gitgateway's own
// parsePath contract).
func parsePath(reqPath string) (route, error) {
	if !strings.HasPrefix(reqPath, PathPrefix) {
		return route{}, fmt.Errorf("apigateway: path %q does not start with %s", reqPath, PathPrefix)
	}
	rest := reqPath[len(PathPrefix):]

	slash1 := strings.IndexByte(rest, '/')
	var token, afterToken string
	if slash1 < 0 {
		token = rest
		afterToken = ""
	} else {
		token = rest[:slash1]
		afterToken = rest[slash1+1:]
	}
	if token == "" {
		return route{}, fmt.Errorf("apigateway: path %q is missing the job-token segment", reqPath)
	}

	slash2 := strings.IndexByte(afterToken, '/')
	var service, tail string
	if slash2 < 0 {
		service = afterToken
		tail = "/"
	} else {
		service = afterToken[:slash2]
		tail = afterToken[slash2:]
	}
	if service == "" {
		return route{}, fmt.Errorf("apigateway: path %q is missing the service segment", reqPath)
	}

	cleaned, err := cleanUpstreamPath(tail)
	if err != nil {
		return route{}, fmt.Errorf("apigateway: path %q: %w", reqPath, err)
	}

	return route{token: token, service: service, path: cleaned}, nil
}

// cleanUpstreamPath normalizes tail (the request path segment after
// /api/<token>/<service>) into an absolute, lexically-clean path with no
// ".." segment left anywhere in it. path.Clean already eliminates every ".."
// element for a rooted (leading "/") input — replacing a leading "/.." with
// "/" rather than climbing above the root — so forcing tail to be absolute
// BEFORE calling Clean is what makes the "cannot escape the service root"
// guarantee hold. The explicit post-Clean scan below is defense in depth:
// under that contract it should never fire, but the function still asserts
// it rather than trusting the contract silently.
func cleanUpstreamPath(tail string) (string, error) {
	if tail == "" {
		tail = "/"
	}
	if !strings.HasPrefix(tail, "/") {
		tail = "/" + tail
	}
	cleaned := path.Clean(tail)
	if cleaned == "." {
		cleaned = "/"
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path escapes the service root")
		}
	}
	return cleaned, nil
}

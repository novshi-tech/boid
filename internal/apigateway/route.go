package apigateway

import (
	"fmt"
	"net/url"
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
	// path is the request tail, EXACTLY as it appeared on the wire (still
	// percent-encoded, always absolute, guaranteed to contain no ".." or "."
	// segment — see parsePath/checkForTraversal). Deliberately NOT
	// path.Clean'd or otherwise normalized: unlike gitgateway (whose paths
	// are a fixed enum of endpoint names), this tail is an arbitrary REST
	// path the caller controls, and cleaning it would silently change what
	// gets forwarded — e.g. path.Clean strips a meaningful trailing slash
	// ("/hooks/" -> "/hooks", a different route on some APIs) and, worse,
	// operating on the DECODED path would turn a "%2F"-encoded slash inside
	// a single path segment (e.g. an object key "a/b" some REST APIs encode
	// as "a%2Fb") into an extra path separator, changing which upstream
	// resource the request actually reaches. Keeping this field's bytes
	// identical to what parsePath received (mechanically split on literal
	// "/", never url-decoded) is what makes forwarding byte-preserving.
	path string
}

// parsePath parses a request path of the form
// /api/<token>/<service>/<path...>, where reqPath is expected to be the
// request's ESCAPED path (r.URL.EscapedPath()) — NOT r.URL.Path. Splitting
// on the escaped form's literal "/" characters is what lets a "%2F" inside a
// single path segment survive as part of that segment rather than being
// silently treated as an extra separator (net/http's own Path field decodes
// %2F into a literal "/" before a handler ever sees it, which would merge
// two logically-distinct segments into three). Every segment is still
// checked for a decoded ".."/"." traversal attempt (checkForTraversal),
// which correctly catches both a literal ".." and an encoded "%2e%2e" form
// either way, since url.PathUnescape is applied per-segment regardless of
// which form parsePath was handed.
//
// Returns an error for any path that doesn't match this exact shape
// (unrecognized routes are treated as 404s by the caller, not 401/403 —
// those statuses are reserved for token/authorization failures on
// well-formed routes, mirroring gitgateway's own parsePath contract).
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
	token, err := url.PathUnescape(token)
	if err != nil || token == "" {
		return route{}, fmt.Errorf("apigateway: path %q has a missing or malformed job-token segment", reqPath)
	}

	slash2 := strings.IndexByte(afterToken, '/')
	var service, tail string
	if slash2 < 0 {
		service = afterToken
		tail = ""
	} else {
		service = afterToken[:slash2]
		tail = afterToken[slash2:]
	}
	service, err = url.PathUnescape(service)
	if err != nil || service == "" {
		return route{}, fmt.Errorf("apigateway: path %q has a missing or malformed service segment", reqPath)
	}

	if !strings.HasPrefix(tail, "/") {
		tail = "/" + tail
	}
	if err := checkForTraversal(tail); err != nil {
		return route{}, fmt.Errorf("apigateway: path %q: %w", reqPath, err)
	}

	return route{token: token, service: service, path: tail}, nil
}

// checkForTraversal rejects tail outright if any "/"-delimited segment
// decodes (via url.PathUnescape) to ".." or ".". tail is expected to
// already be absolute (leading "/"). Unlike gitgateway's sibling guard,
// this does NOT attempt to resolve/clean the path (path.Clean-style
// collapsing of ".."/"." — see route.path's own doc comment for why that
// would itself change what gets forwarded); it only ever accepts a tail
// unchanged or rejects it outright, so a legitimate path is always
// byte-identical on the way out. A malformed percent-encoding in any
// segment is rejected the same way, rather than silently passed through.
func checkForTraversal(tail string) error {
	for _, seg := range strings.Split(tail, "/") {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return fmt.Errorf("malformed percent-encoding in path segment %q", seg)
		}
		if decoded == ".." || decoded == "." {
			return fmt.Errorf("path segment %q attempts to escape the service root", seg)
		}
	}
	return nil
}

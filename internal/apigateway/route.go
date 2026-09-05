package apigateway

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// PathPrefix is the fixed prefix every gateway route starts with:
// /api/<job-token>/<service>/<path...>.
const PathPrefix = "/api/"

// route is the parsed shape of a gateway request path.
type route struct {
	token   string
	service string
	// account is the optional credential-account qualifier embedded in the
	// service path segment ("<service>@<account>"), already split off of
	// service by parsePath. Empty when the request named no account.
	account string
	// path is the request tail, EXACTLY as it appeared on the wire (still
	// percent-encoded, always absolute, guaranteed to contain no ".." or "."
	// segment — see parsePath/checkForTraversal). Deliberately NOT
	// path.Clean'd or otherwise normalized: this tail is an arbitrary REST
	// path the caller controls, and cleaning it would silently change what
	// gets forwarded — e.g. path.Clean strips a meaningful trailing slash,
	// and operating on the DECODED path would turn a "%2F"-encoded slash
	// inside a single path segment into an extra separator, changing which
	// upstream resource the request actually reaches. Keeping this field's
	// bytes identical to what parsePath received is what makes forwarding
	// byte-preserving.
	path string
}

// parsePath parses a request path of the form
// /api/<token>/<service>/<path...>, where reqPath is expected to be the
// request's ESCAPED path (r.URL.EscapedPath()) — NOT r.URL.Path. Splitting
// on the escaped form's literal "/" characters is what lets a "%2F" inside a
// single path segment survive as part of that segment rather than being
// silently treated as an extra separator (net/http's own Path field decodes
// %2F into a literal "/" before a handler ever sees it, which would merge
// two logically-distinct segments into three).
//
// Returns an error for any path that doesn't match this exact shape
// (unrecognized routes are treated as 404s by the caller, not 401/403 —
// those statuses are reserved for token/authorization failures on
// well-formed routes).
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

	// account splitting happens on the DECODED service segment — see
	// splitServiceAccount's own doc comment for why.
	svcName, account, err := splitServiceAccount(service)
	if err != nil {
		return route{}, fmt.Errorf("apigateway: path %q: %w", reqPath, err)
	}

	if !strings.HasPrefix(tail, "/") {
		tail = "/" + tail
	}
	if err := checkForTraversal(tail); err != nil {
		return route{}, fmt.Errorf("apigateway: path %q: %w", reqPath, err)
	}

	return route{token: token, service: svcName, account: account, path: tail}, nil
}

// errInvalidAccount marks the one parsePath failure class that must map to
// HTTP 400, not 404: the path IS the well-formed
// /api/<token>/<service>/<tail> shape — the service segment is present and
// non-empty — but the account name embedded in it ("<service>@<account>")
// fails validation. Every OTHER parsePath error means the path doesn't
// match that shape at all and stays a 404. Server.ServeHTTP tells the two
// apart with errors.Is.
var errInvalidAccount = errors.New("apigateway: invalid account name in service segment")

// splitServiceAccount splits a service path segment into its base service
// name and optional account qualifier, on "@". It is called on the segment
// AFTER url.PathUnescape has already run, deliberately: config load already
// rejects "@" in any real service/provider name, so a decoded "@" is
// unambiguous evidence of an account qualifier, and a literal "@" and its
// "%40" encoding should mean the same thing here (unlike route.path, which
// is never re-decoded — see its own doc comment).
//
// Returns errInvalidAccount (wrapped, with detail) for: more than one "@"
// in the segment, an empty base name before "@", or an account name that
// fails validateAccountName. A segment with no "@" at all is valid and
// returns account == "".
func splitServiceAccount(service string) (name, account string, err error) {
	parts := strings.Split(service, "@")
	switch len(parts) {
	case 1:
		return service, "", nil
	case 2:
		name, account = parts[0], parts[1]
	default:
		return "", "", fmt.Errorf("service segment %q has %d \"@\" characters, want at most 1: %w", service, len(parts)-1, errInvalidAccount)
	}
	if name == "" {
		return "", "", fmt.Errorf("service segment %q has no base service name before \"@\": %w", service, errInvalidAccount)
	}
	if err := validateAccountName(account); err != nil {
		return "", "", fmt.Errorf("service segment %q: %w", service, err)
	}
	return name, account, nil
}

// validateAccountName enforces the account name character set:
// alphanumeric, "-", "_" only, 1-64 characters. "@"/"/"/":" are rejected by
// construction, since they would otherwise collide with, respectively, the
// account separator itself, path-segment splitting, and secret-key/cache-key
// construction.
func validateAccountName(account string) error {
	if account == "" {
		return fmt.Errorf("account name must not be empty: %w", errInvalidAccount)
	}
	if len(account) > 64 {
		return fmt.Errorf("account name %q is %d characters, want at most 64: %w", account, len(account), errInvalidAccount)
	}
	for _, r := range account {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '-' && r != '_' {
			return fmt.Errorf("account name %q contains %q, which is not one of [A-Za-z0-9_-]: %w", account, string(r), errInvalidAccount)
		}
	}
	return nil
}

// ValidateAccountName is validateAccountName's exported form, so that
// callers outside this package (e.g. `boid secret oauth login --account`)
// can gate an account name on the identical rule parsePath's
// splitServiceAccount enforces, rather than risk a login that creates a
// credential the gateway can never route a request to.
//
// Like validateAccountName itself, this rejects "" — a caller for whom an
// empty string legitimately means "no account requested" must check
// account != "" itself before calling this.
func ValidateAccountName(account string) error {
	return validateAccountName(account)
}

// checkForTraversal rejects tail outright if any "/"-delimited segment
// decodes (via url.PathUnescape) to ".." or ".", INCLUDING when that ".."
// or "." only appears after decoding an ENCODED slash ("%2f"/"%2F") inside
// what checkForTraversal itself treats as a single raw segment — e.g. the
// raw segment "%2e%2e%2fadmin" (no literal "/" in it, so it is one segment
// by the outer split) decodes to "../admin", which itself contains a "/"
// and must be split and checked again. This is why the check re-splits
// DECODED content and inspects every resulting sub-segment, not just the
// outer one: some upstreams decode "%2F" themselves before routing, so a
// segment forwarded intact (never treated as a separator on this side)
// could still resolve to a genuine ".." traversal once the upstream's own
// decoding runs on it.
//
// tail is expected to already be absolute (leading "/"). This does NOT
// attempt to resolve/clean the path (see route.path's own doc comment for
// why); it only ever accepts a tail unchanged or rejects it outright, so a
// legitimate path is always byte-identical on the way out. A malformed
// percent-encoding in any segment is rejected the same way, rather than
// silently passed through.
func checkForTraversal(tail string) error {
	for _, seg := range strings.Split(tail, "/") {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return fmt.Errorf("malformed percent-encoding in path segment %q", seg)
		}
		for _, subSeg := range strings.Split(decoded, "/") {
			if subSeg == ".." || subSeg == "." {
				return fmt.Errorf("path segment %q (decodes to %q) attempts to escape the service root", seg, decoded)
			}
		}
	}
	return nil
}

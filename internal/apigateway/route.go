package apigateway

import (
	"errors"
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
	// account is the optional credential-account qualifier embedded in the
	// service path segment ("<service>@<account>" — docs/plans/
	// api-gateway-credential-accounts.md D1/D2). Empty when the request
	// named no account, in which case every downstream decision (authz,
	// BaseURL, credential resolution) is byte-identical to pre-account-
	// support behavior — service alone is ALWAYS the base name with any
	// "@<account>" already stripped off by parsePath, never the raw
	// "<service>@<account>" segment; account carries that qualifier
	// separately so callers that must ignore it (authorization/BaseURL/
	// readonly-write lookups, D4) and callers that must use it (credential
	// resolution, D2/D3) each read the field they need without
	// re-splitting the string themselves.
	account string
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

	// account splitting happens on the DECODED service segment (docs/plans/
	// api-gateway-credential-accounts.md D1/D11) — see splitServiceAccount's
	// own doc comment for why operating post-unescape (rather than
	// splitting the raw segment on a literal "@" before unescaping it) is
	// the correct choice here, unlike route.path's byte-preservation
	// requirement.
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
// HTTP 400, not 404 (docs/plans/api-gateway-credential-accounts.md D11):
// the path IS the well-formed /api/<token>/<service>/<tail> shape — the
// service segment is present and non-empty — but the account name embedded
// in it ("<service>@<account>") fails validation. Every OTHER parsePath
// error means the path doesn't match that shape at all (missing segment,
// malformed percent-encoding, traversal attempt, ...) and stays a 404,
// matching this function's long-standing contract (see its own doc
// comment). Server.ServeHTTP tells the two apart with errors.Is.
var errInvalidAccount = errors.New("apigateway: invalid account name in service segment")

// splitServiceAccount splits a service path segment into its base service
// name and optional account qualifier, on "@" (docs/plans/
// api-gateway-credential-accounts.md D1). It is called on the segment
// AFTER url.PathUnescape has already run (parsePath's own call site) —
// deliberately, for two reasons:
//
//   - config load (internal/config's validateServiceConfig) rejects "@" in
//     every services.<name> and oauth_providers.<name> value, so a decoded
//     segment containing "@" can never collide with a legitimate
//     unqualified service name — "@" appearing post-decode is unambiguous
//     evidence of an account qualifier, never part of a real service name.
//   - a literal "@" and its "%40" percent-encoded form become the exact
//     same string once both are unescaped, and there is no reason a caller
//     would want the two to mean something different here — treating them
//     identically is also simply what "split after unescape" gives for
//     free, with no special-casing needed. (Contrast this with route.path,
//     which is deliberately NEVER unescaped or re-split — see its own doc
//     comment — because an upstream REST resource path can legitimately
//     use "%2F" to mean something other than a literal separator; a
//     service NAME has no such legitimate use for a raw "@" that config
//     load doesn't already forbid.)
//
// Returns errInvalidAccount (wrapped, with detail) for: more than one "@"
// in the segment, an empty base name before "@", or an account name that
// fails validateAccountName. A segment with no "@" at all is valid and
// returns account == "" (D2 — account-less requests are parsed identically
// to before this feature existed).
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

// validateAccountName enforces D11's account name character set:
// alphanumeric, "-", "_" only, 1-64 characters. "@"/"/"/":" are rejected by
// construction (they are not in the allowed set) — D11 calls those out
// specifically because they would otherwise collide with, respectively, the
// account separator itself, path-segment splitting, and secret-key/cache-key
// construction (docs/plans/api-gateway-credential-accounts.md's credentialID
// discussion, PR-2).
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

// checkForTraversal rejects tail outright if any "/"-delimited segment
// decodes (via url.PathUnescape) to ".." or ".", INCLUDING when that ".."
// or "." only appears after decoding an ENCODED slash ("%2f"/"%2F") inside
// what checkForTraversal itself treats as a single raw segment — e.g. the
// raw segment "%2e%2e%2fadmin" (no literal "/" in it, so it is one segment
// by the outer split) decodes to "../admin", which itself contains a "/"
// and must be split and checked again (codex review round 2 finding: a
// naive whole-segment `decoded == ".."` equality check does not catch this,
// since the fully-decoded string is "../admin", not "..").
//
// This is why the check below re-splits DECODED content and inspects every
// resulting sub-segment, not just the outer (still largely raw) one: some
// upstreams DO decode "%2F" themselves before routing (reverse proxies,
// web frameworks, CDNs), so a request this gateway forwards with an intact
// "%2e%2e%2fadmin" segment (never treated as a separator on THIS side,
// which is what makes the earlier "%2F must be preserved as one segment"
// feature correct) could still resolve to a genuine ".." traversal once
// the UPSTREAM'S OWN decoding runs on it. Rejecting outright here is what
// keeps the "cannot escape the service root" guarantee airtight for both
// interpretations at once.
//
// tail is expected to already be absolute (leading "/"). Unlike gitgateway's
// sibling guard, this does NOT attempt to resolve/clean the path
// (path.Clean-style collapsing of ".."/"." — see route.path's own doc
// comment for why that would itself change what gets forwarded); it only
// ever accepts a tail unchanged or rejects it outright, so a legitimate
// path is always byte-identical on the way out. A malformed
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

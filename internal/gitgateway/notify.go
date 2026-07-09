package gitgateway

// UpstreamAuthFailureNotifier is invoked on two distinct failure modes so an
// operator can tell them apart (docs/plans/git-gateway-cutover.md PR4,
// flagged in PR3's review: 「config error (認証注入失敗) と token 失効を
// differentiate できると良い」):
//
//   - NotifyUpstreamAuthFailure: the upstream forge responded with 401 to a
//     request that WAS sent with injected credentials — the token itself is
//     wrong/expired/revoked ("rotate the token"; docs/plans/container-based-boid.md
//     「token 戦略」: 「失効前提の運用」。両 forge とも token は失効前提).
//   - NotifyCredentialError: credential injection itself failed before the
//     request ever reached the upstream — an unconfigured host, a missing
//     resolver, or a secret-store lookup error ("fix the gateway config").
//
// The gateway always logs a warning on either condition and additionally
// calls the matching hook, but never aborts serving other requests because
// of it (docs/plans/git-gateway-cutover.md: 「gateway 自体は落とさない」).
type UpstreamAuthFailureNotifier interface {
	NotifyUpstreamAuthFailure(host string, repo RepoKey)
	NotifyCredentialError(host string, repo RepoKey, err error)
}

// NotifierFuncs adapts plain functions to UpstreamAuthFailureNotifier. Either
// field may be left nil, in which case the corresponding notification is a
// no-op — callers that only care about one of the two failure modes (e.g.
// tests) do not need to stub both.
type NotifierFuncs struct {
	UpstreamAuthFailure func(host string, repo RepoKey)
	CredentialError     func(host string, repo RepoKey, err error)
}

// NotifyUpstreamAuthFailure implements UpstreamAuthFailureNotifier.
func (f NotifierFuncs) NotifyUpstreamAuthFailure(host string, repo RepoKey) {
	if f.UpstreamAuthFailure != nil {
		f.UpstreamAuthFailure(host, repo)
	}
}

// NotifyCredentialError implements UpstreamAuthFailureNotifier.
func (f NotifierFuncs) NotifyCredentialError(host string, repo RepoKey, err error) {
	if f.CredentialError != nil {
		f.CredentialError(host, repo, err)
	}
}

// NoopNotifier discards notifications. It is the default when a Server is
// constructed with a nil notifier.
var NoopNotifier UpstreamAuthFailureNotifier = NotifierFuncs{}

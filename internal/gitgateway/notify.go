package gitgateway

// UpstreamAuthFailureNotifier is invoked when an upstream forge responds
// with 401 to a proxied request — taken as a sign that the configured token
// has expired or been revoked (docs/plans/container-based-boid.md 「token
// 戦略」: 「失効前提の運用」。両 forge とも token は失効前提). The gateway
// always logs a warning on 401 and additionally calls this hook, but never
// aborts serving other requests because of it
// (docs/plans/git-gateway-cutover.md: 「gateway 自体は落とさない」).
//
// PR3 only defines this interface ("何らかの notify hook (interface だけ
// 用意)"); a later PR wires a concrete implementation backed by
// internal/notify.
type UpstreamAuthFailureNotifier interface {
	NotifyUpstreamAuthFailure(host string, repo RepoKey)
}

// UpstreamAuthFailureNotifierFunc adapts a plain function to
// UpstreamAuthFailureNotifier.
type UpstreamAuthFailureNotifierFunc func(host string, repo RepoKey)

// NotifyUpstreamAuthFailure implements UpstreamAuthFailureNotifier.
func (f UpstreamAuthFailureNotifierFunc) NotifyUpstreamAuthFailure(host string, repo RepoKey) {
	f(host, repo)
}

// NoopNotifier discards notifications. It is the default when a Server is
// constructed with a nil notifier.
var NoopNotifier UpstreamAuthFailureNotifier = UpstreamAuthFailureNotifierFunc(func(string, RepoKey) {})

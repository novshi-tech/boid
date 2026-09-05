package apigateway

// UpstreamAuthFailureNotifier is invoked on two distinct failure modes so an
// operator can tell them apart:
//
//   - NotifyUpstreamAuthFailure: the upstream service responded with 401 to
//     a request that WAS sent with injected credentials — the configured
//     secret itself is wrong/expired/revoked ("rotate it with `boid secret
//     set`").
//   - NotifyCredentialError: credential injection itself failed before the
//     request ever reached the upstream — an unconfigured service, a
//     missing resolver, or a secret-store lookup error ("fix config.yaml /
//     boid secret set").
//
// The gateway always logs a warning on either condition and additionally
// calls the matching hook, but never aborts serving other requests because
// of it.
type UpstreamAuthFailureNotifier interface {
	NotifyUpstreamAuthFailure(service string)
	NotifyCredentialError(service string, err error)
}

// NotifierFuncs adapts plain functions to UpstreamAuthFailureNotifier.
// Either field may be left nil, in which case the corresponding notification
// is a no-op.
type NotifierFuncs struct {
	UpstreamAuthFailure func(service string)
	CredentialError     func(service string, err error)
}

// NotifyUpstreamAuthFailure implements UpstreamAuthFailureNotifier.
func (f NotifierFuncs) NotifyUpstreamAuthFailure(service string) {
	if f.UpstreamAuthFailure != nil {
		f.UpstreamAuthFailure(service)
	}
}

// NotifyCredentialError implements UpstreamAuthFailureNotifier.
func (f NotifierFuncs) NotifyCredentialError(service string, err error) {
	if f.CredentialError != nil {
		f.CredentialError(service, err)
	}
}

// NoopNotifier discards notifications. It is the default when a Server is
// constructed with a nil notifier.
var NoopNotifier UpstreamAuthFailureNotifier = NotifierFuncs{}

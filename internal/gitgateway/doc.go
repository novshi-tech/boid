// Package gitgateway implements the authenticating reverse proxy that sits
// between a sandbox's credential-less git and the real upstream forge
// (GitHub / Bitbucket).
//
// The package purposefully avoids importing internal/dispatcher or
// internal/db so it can build and test standalone; secret resolution and
// notification are expressed as small function-typed seams (SecretResolver,
// UpstreamAuthFailureNotifier) that callers adapt to the real
// internal/dispatcher.SecretStore and internal/notify.
package gitgateway

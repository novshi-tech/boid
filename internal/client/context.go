package client

import "context"

// clientCtxKey is the unexported context key root's PersistentPreRunE uses
// to inject the profile-resolved Client for a single command invocation
// (docs/plans/cli-remote-connection.md PR1 "root PersistentPreRunE の
// profile 解決 + client 注入"). Unexported so nothing outside this package
// can collide with it or read/write it by any means other than
// WithClient/FromContext.
type clientCtxKey struct{}

// WithClient returns a copy of ctx carrying c as the resolved client for
// this invocation. A nil ctx (cobra's own zero value before
// Execute()/SetContext ever run — see cmd/root.go's PersistentPreRunE doc
// comment for why that is the normal case for the leaf command it is
// called with) is treated as context.Background() instead of panicking:
// context.WithValue itself panics on a nil parent.
func WithClient(ctx context.Context, c *Client) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientCtxKey{}, c)
}

// FromContext returns the Client injected into ctx by root's
// PersistentPreRunE. If ctx is nil, or carries no client at all — every
// call that bypasses cobra's Execute()/PersistentPreRunE lifecycle, which
// today includes every cmd package test that invokes a runXxx(cmd, args)
// function directly against a freshly constructed *cobra.Command — this
// falls back to NewUnixClient(DefaultSocketPath()), this CLI's long-
// standing (and, until Phase 3, only) default. That fallback is
// deliberately identical to what profiles.Resolve itself produces when no
// config.yaml/--profile/BOID_PROFILE/default_profile is set at all
// (docs/plans/cli-remote-connection.md's "現行互換" contract), so the two
// paths never diverge for that common case, and this function never needs
// to return an error.
func FromContext(ctx context.Context) *Client {
	if ctx != nil {
		if c, ok := ctx.Value(clientCtxKey{}).(*Client); ok && c != nil {
			return c
		}
	}
	return NewUnixClient(DefaultSocketPath())
}

// FromContextOrNil returns the Client injected into ctx by root's
// PersistentPreRunE, or nil if none was injected. Unlike FromContext,
// this does NOT fall back to the unix default — used by shell TAB
// completion (cmd/completion.go's completeProjectRefs) so a failed
// profile resolution degrades to "no candidates" instead of silently
// querying whichever daemon happens to be listening on the local unix
// socket. Foreground CLI commands keep using FromContext, whose
// fallback matches the "現行互換" contract profiles.Resolve itself
// produces when no profile is configured.
func FromContextOrNil(ctx context.Context) *Client {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(clientCtxKey{}).(*Client)
	return c
}

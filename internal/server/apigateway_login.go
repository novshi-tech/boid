package server

import (
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
)

// apiGatewayLoginAdapter adapts apigateway.CredentialProvider (the
// service->provider lookup) and apigateway.LoginManager (the actual device/
// loopback/manual flow machinery) to api.OAuthLoginService — the daemon-side
// backing for `boid secret oauth login <service>` (docs/plans/api-gateway.md
// §7, PR3). A thin translation layer only: every real decision (flow
// selection, PKCE/state, persistence order) lives in internal/apigateway/
// login.go; this file exists purely so internal/api's handler stays free of
// any internal/apigateway import (mirroring apiGatewayNotifier/
// newAPIGatewayRecorder's identical role in apigateway_notify.go).
type apiGatewayLoginAdapter struct {
	creds  *apigateway.CredentialProvider
	logins *apigateway.LoginManager
}

// ProviderForService implements api.OAuthLoginService.
func (a *apiGatewayLoginAdapter) ProviderForService(service string) (string, bool) {
	return a.creds.OAuth2ProviderFor(service)
}

// KnowsProvider implements api.OAuthLoginService.
func (a *apiGatewayLoginAdapter) KnowsProvider(name string) bool {
	return a.logins.KnowsProvider(name)
}

// StartLogin implements api.OAuthLoginService, translating
// apigateway.LoginStart into api.OAuthLoginStart's own (identically shaped,
// independently typed) DTO.
func (a *apiGatewayLoginAdapter) StartLogin(namespace, provider, redirectURI string) (*api.OAuthLoginStart, error) {
	start, err := a.logins.StartLogin(namespace, provider, redirectURI)
	if err != nil {
		return nil, err
	}
	return &api.OAuthLoginStart{
		SessionID:               start.SessionID,
		Flow:                    string(start.Flow),
		AuthorizeURL:            start.AuthorizeURL,
		UserCode:                start.UserCode,
		VerificationURI:         start.VerificationURI,
		VerificationURIComplete: start.VerificationURIComplete,
		IntervalSeconds:         start.IntervalSeconds,
		ExpiresInSeconds:        start.ExpiresInSeconds,
	}, nil
}

// CompleteLogin implements api.OAuthLoginService.
func (a *apiGatewayLoginAdapter) CompleteLogin(sessionID, code, state string) error {
	return a.logins.CompleteLogin(sessionID, code, state)
}

// LoginStatus implements api.OAuthLoginService.
func (a *apiGatewayLoginAdapter) LoginStatus(sessionID string) (status, errMsg string, ok bool) {
	s, msg, ok := a.logins.Status(sessionID)
	return string(s), msg, ok
}

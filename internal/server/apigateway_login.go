package server

import (
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
)

// apiGatewayLoginAdapter adapts apigateway.CredentialProvider and
// apigateway.LoginManager to api.OAuthLoginService — the daemon-side backing
// for `boid secret oauth login <service>`. A thin translation layer only;
// the actual login logic lives in internal/apigateway/login.go.
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
// apigateway.LoginStart into api.OAuthLoginStart's DTO.
func (a *apiGatewayLoginAdapter) StartLogin(namespace, provider, redirectURI, account string) (*api.OAuthLoginStart, error) {
	start, err := a.logins.StartLogin(namespace, provider, redirectURI, account)
	if err != nil {
		return nil, err
	}
	return &api.OAuthLoginStart{
		SessionID:               start.SessionID,
		Flow:                    string(start.Flow),
		Account:                 start.Account,
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

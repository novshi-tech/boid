package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// OAuthLoginService is the daemon-side surface `boid secret oauth login
// <service>` (docs/plans/api-gateway.md §7, PR3) drives, implemented by
// internal/server/wire.go's adapter over apigateway.CredentialProvider (the
// service->provider lookup) and apigateway.LoginManager (the actual
// device/loopback/manual flow machinery) — kept as a narrow interface
// (mirroring api.SecretStore/api.WorkspaceHomeStore's existing convention)
// so this handler's own tests can substitute a fake without a real OAuth2
// token endpoint or secret store.
//
// Every method's return shape is deliberately untyped-error-only (a plain
// string, no distinguishable error codes) except where a boolean "ok" is
// structurally derivable without string sniffing (ProviderForService,
// LoginStatus) — see each handler method's own comment for how that maps to
// HTTP status codes.
type OAuthLoginService interface {
	// ProviderForService resolves a config.yaml services.<name> entry to
	// the oauth_providers.<name> its auth.kind: oauth2 config references.
	// ok is false when service is unknown OR configured with any auth.kind
	// other than oauth2 (mirrors apigateway.CredentialProvider.
	// OAuth2ProviderFor exactly — the adapter just forwards to it).
	ProviderForService(service string) (provider string, ok bool)
	// StartLogin begins a new login session — see
	// apigateway.LoginManager.StartLogin's own doc comment for the full
	// per-flow contract (redirectURI is only meaningful for the loopback
	// flow; the CLI always supplies it regardless of which flow the
	// service turns out to use).
	StartLogin(namespace, provider, redirectURI string) (*OAuthLoginStart, error)
	// CompleteLogin finishes a loopback or manual session — see
	// apigateway.LoginManager.CompleteLogin's own doc comment (state is
	// ignored for manual, which never had one to check).
	CompleteLogin(sessionID, code, state string) error
	// LoginStatus reports a session's current lifecycle state — ok is
	// false when sessionID is unknown.
	LoginStatus(sessionID string) (status, errMsg string, ok bool)
}

// OAuthLoginStart is the internal/api-owned mirror of
// apigateway.LoginStart — internal/api deliberately does not import
// internal/apigateway's own type here (this package's DTOs are the wire
// contract; the adapter in internal/server/wire.go is what actually touches
// apigateway types), keeping this handler's own tests free of any
// apigateway/oauth2-endpoint dependency, the same "handler depends on a
// narrow service interface with its own DTOs, not a concrete backend
// package's types" convention api.SecretHandler/api.WorkspaceHandler follow.
type OAuthLoginStart struct {
	SessionID               string
	Flow                    string
	AuthorizeURL            string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	IntervalSeconds         int
	ExpiresInSeconds        int
}

// OAuthLoginHandler serves `boid secret oauth login <service>`'s three
// daemon endpoints under whatever prefix internal/server/wire.go mounts it
// at (POST .../login, GET .../login/{id}, POST .../login/{id}/complete).
type OAuthLoginHandler struct {
	Service OAuthLoginService
}

func (h *OAuthLoginHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/login", h.Start)
	r.Get("/login/{id}", h.Status)
	r.Post("/login/{id}/complete", h.Complete)
	return r
}

type oauthLoginStartRequest struct {
	Service string `json:"service"`
	// Namespace defaults to "default" — same convention as
	// api.secretSetRequest/secretNamespace (secret.go).
	Namespace string `json:"namespace"`
	// RedirectURI is the CLI's freshly-opened RFC 8252 §7.3 loopback
	// listener's callback URL, ALWAYS supplied by the CLI regardless of
	// which flow the service turns out to use — see LoginManager.
	// StartLogin's own doc comment for why a single request suffices for
	// every flow this way.
	RedirectURI string `json:"redirect_uri,omitempty"`
}

type oauthLoginStartResponse struct {
	SessionID               string `json:"session_id"`
	Flow                    string `json:"flow"`
	AuthorizeURL            string `json:"authorize_url,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	IntervalSeconds         int    `json:"interval_seconds,omitempty"`
	ExpiresInSeconds        int    `json:"expires_in_seconds,omitempty"`
}

// Start implements POST .../login. Every failure here is reported as 400
// (the request as given cannot be satisfied right now — an unrecognized
// flow config, a device_authorization_endpoint call failing, ...) except
// two structurally-derivable cases: h.Service == nil (503 — the daemon has
// no secret store/OAuth2 machinery configured at all, mirroring
// SecretHandler's own systemic-unavailability convention) and
// ProviderForService reporting ok=false (404 — service is genuinely
// unknown or not an oauth2-kind service, a structural fact this handler
// can check without parsing any error string).
func (h *OAuthLoginHandler) Start(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth2 login is not available (no secret store configured)")
		return
	}
	var req oauthLoginStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Service == "" {
		writeError(w, http.StatusBadRequest, "service is required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	provider, ok := h.Service.ProviderForService(req.Service)
	if !ok {
		writeError(w, http.StatusNotFound, "service "+req.Service+" is not configured with auth.kind: oauth2")
		return
	}
	start, err := h.Service.StartLogin(req.Namespace, provider, req.RedirectURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, oauthLoginStartResponse{
		SessionID:               start.SessionID,
		Flow:                    start.Flow,
		AuthorizeURL:            start.AuthorizeURL,
		UserCode:                start.UserCode,
		VerificationURI:         start.VerificationURI,
		VerificationURIComplete: start.VerificationURIComplete,
		IntervalSeconds:         start.IntervalSeconds,
		ExpiresInSeconds:        start.ExpiresInSeconds,
	})
}

type oauthLoginStatusResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Status implements GET .../login/{id} — the CLI's device-flow poll (docs/
// plans/api-gateway.md §7: the daemon polls the real provider in the
// background; the CLI only ever polls THIS cheap, local, daemon-session
// check). 404 when the session id is unknown — ok=false is a structural
// fact from LoginStatus, never a parsed error string.
func (h *OAuthLoginHandler) Status(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth2 login is not available (no secret store configured)")
		return
	}
	id := chi.URLParam(r, "id")
	status, errMsg, ok := h.Service.LoginStatus(id)
	if !ok {
		writeError(w, http.StatusNotFound, "login session not found or expired")
		return
	}
	writeJSON(w, http.StatusOK, oauthLoginStatusResponse{Status: status, Error: errMsg})
}

type oauthLoginCompleteRequest struct {
	Code string `json:"code"`
	// State is required (and checked) for the loopback flow only — see
	// apigateway.LoginManager.CompleteLogin's own doc comment. Empty for
	// manual, which never had a state to check.
	State string `json:"state,omitempty"`
}

// Complete implements POST .../login/{id}/complete — the loopback flow's
// browser-callback code (forwarded by the CLI's local listener) or the
// manual flow's user-pasted OOB code. Every CompleteLogin failure
// (unknown/expired/already-completed session, state mismatch, upstream
// exchange failure, ...) is reported as 400 — see Start's own doc comment
// for why this handler does not attempt to distinguish these from
// LoginManager's plain error strings.
func (h *OAuthLoginHandler) Complete(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth2 login is not available (no secret store configured)")
		return
	}
	id := chi.URLParam(r, "id")
	var req oauthLoginCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := h.Service.CompleteLogin(id, req.Code, req.State); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "complete"})
}

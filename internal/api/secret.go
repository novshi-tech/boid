package api

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

type SecretStore interface {
	List(namespace string) ([]string, error)
	Set(namespace, key, value string) error
	Delete(namespace, key string) error
	Get(namespace, key string) (string, error)
}

type SecretHandler struct {
	Store SecretStore
}

func (h *SecretHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Set)
	// {key:.*} rather than the plain {key} chi shorthand: a bare {key}
	// only ever captures a single path segment, so a key containing "/"
	// (e.g. atl-cli's "<site-alias>/api-token" shape, the exact form
	// internal/auth.SaveSite in novshi-tech/atl-cli uses) 404s here no
	// matter how it is escaped on the way in — Set (JSON body) stores it
	// fine, but GetValue/Delete can never look it back up. See
	// TestSecretHandler_KeyWithSlash_GetAndDeleteRoundTrip for the
	// regression this fixes (found via atl's boid-cli credential backend,
	// production dogfood 2026-07-29).
	r.Delete("/{key:.*}", h.Delete)
	r.Get("/{key:.*}/value", h.GetValue)
	return r
}

func secretNamespace(r *http.Request) string {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		return "default"
	}
	return ns
}

func (h *SecretHandler) List(w http.ResponseWriter, r *http.Request) {
	keys, err := h.Store.List(secretNamespace(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, keys)
}

type secretSetRequest struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

func (h *SecretHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req secretSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Key == "" || req.Value == "" {
		writeError(w, http.StatusBadRequest, "key and value required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if err := h.Store.Set(req.Namespace, req.Key, req.Value); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// secretKeyParam extracts and unescapes the "key" path parameter. chi's
// router matches routes against the request's RAW (still percent-encoded)
// path — see (*chi.Mux).routeHTTP's RawPath-before-Path fallback — so
// chi.URLParam hands the handler "verify-test%2Fapi-token" for a key like
// "verify-test/api-token" (cmd/secret.go's runSecretGet/runSecretDelete
// always url.PathEscape the key before building the request, precisely so
// a "/"-containing key — e.g. atl-cli's "<site-alias>/api-token" shape —
// survives the trip as ONE path segment rather than being split). Passing
// that escaped form straight to SecretStore looks up a key that was never
// stored: Set (JSON body, no URL involved) always stores the key
// unescaped. Unescaping here is what makes Get/Delete find the same row
// Set wrote. See TestSecretHandler_KeyWithSlash_GetAndDeleteRoundTrip.
func secretKeyParam(r *http.Request) (string, error) {
	return url.PathUnescape(chi.URLParam(r, "key"))
}

func (h *SecretHandler) Delete(w http.ResponseWriter, r *http.Request) {
	key, err := secretKeyParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid key path parameter: "+err.Error())
		return
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "key path parameter required")
		return
	}
	if err := h.Store.Delete(secretNamespace(r), key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *SecretHandler) GetValue(w http.ResponseWriter, r *http.Request) {
	key, err := secretKeyParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid key path parameter: "+err.Error())
		return
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "key path parameter required")
		return
	}
	val, err := h.Store.Get(secretNamespace(r), key)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"value": val})
}

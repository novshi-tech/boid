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
	// {key:.*} rather than the plain {key} chi shorthand: a bare {key} only
	// captures a single path segment, so a key containing "/" would 404
	// here no matter how it's escaped, even though Set (JSON body) stores
	// it fine.
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

// secretKeyParam extracts and unescapes the "key" path parameter. chi
// matches routes against the raw (still percent-encoded) request path, so
// chi.URLParam returns the escaped form for a "/"-containing key (callers
// url.PathEscape it so it survives as one path segment). Unescaping here is
// what makes Get/Delete find the same row Set (never escaped) wrote.
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

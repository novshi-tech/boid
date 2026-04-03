package api

import (
	"encoding/json"
	"net/http"

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
	r.Delete("/{key}", h.Delete)
	r.Get("/{key}/value", h.GetValue)
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

func (h *SecretHandler) Delete(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
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
	key := chi.URLParam(r, "key")
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

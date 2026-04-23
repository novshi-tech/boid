package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/web/templates"
)

type WebHandler struct {
	Service WebService
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	r.Get("/projects", h.ProjectList)
	r.Get("/jobs", h.JobList)
	r.Get("/jobs/{id}", h.JobDetail)
	return r
}

func (h *WebHandler) TaskList(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	tasks, err := h.Service.ListTasks(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskList(tasks, filter).Render(r.Context(), w)
}

func (h *WebHandler) TaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	jobs := make([]*templates.JobView, 0, len(detail.Jobs))
	for _, job := range detail.Jobs {
		jobs = append(jobs, &templates.JobView{
			ID:        job.ID,
			HandlerID: job.HandlerID,
			Role:      job.Role,
			Status:    string(job.Status),
			ExitCode:  job.ExitCode,
			CreatedAt: job.CreatedAt,
			UpdatedAt: job.UpdatedAt,
			Output:    job.Output,
		})
	}
	errorMsg := r.URL.Query().Get("error")
	templates.TaskDetail(detail.Task, detail.Actions, jobs, detail.AvailableActions, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actionType := r.FormValue("type")
	if actionType == "" {
		http.Redirect(w, r, "/tasks/"+id+"?error=type+is+required", http.StatusSeeOther)
		return
	}
	if err := h.Service.ApplyAction(id, actionType); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

func (h *WebHandler) PostDuplicate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	newID, err := h.Service.DuplicateTask(id)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+newID, http.StatusSeeOther)
}

func (h *WebHandler) ProjectList(w http.ResponseWriter, r *http.Request) {
	projects, err := h.Service.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ProjectList(projects).Render(r.Context(), w)
}

func (h *WebHandler) JobList(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	jobs, err := h.Service.ListJobs(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]*templates.JobContextView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, &templates.JobContextView{
			ID:        j.ID,
			TaskID:    j.TaskID,
			TaskTitle: j.TaskTitle,
			HandlerID: j.HandlerID,
			Role:      j.Role,
			Status:    string(j.Status),
			ExitCode:  j.ExitCode,
			CreatedAt: j.CreatedAt,
			UpdatedAt: j.UpdatedAt,
			Output:    j.Output,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobList(views, filter).Render(r.Context(), w)
}

func (h *WebHandler) JobDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Service.GetJob(id)
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	view := &templates.JobContextView{
		ID:        job.ID,
		TaskID:    job.TaskID,
		TaskTitle: job.TaskTitle,
		HandlerID: job.HandlerID,
		Role:      job.Role,
		Status:    string(job.Status),
		ExitCode:  job.ExitCode,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
		Output:    job.Output,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobDetail(view).Render(r.Context(), w)
}

// WebManagementHandler serves the CLI management API at /api/web/*.
// All routes are accessible only via UNIX socket (CLI control plane).
type WebManagementHandler struct {
	Pairing   *auth.PairingManager
	Store     *auth.Store
	PublicURL string
}

func (h *WebManagementHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/pair", h.PostPair)
	r.Get("/devices", h.GetDevices)
	r.Delete("/devices/{id}", h.DeleteDevice)
	r.Delete("/devices", h.DeleteAllDevices)
	return r
}

type pairResponse struct {
	Code      string `json:"code"`
	URL       string `json:"url,omitempty"`
	ExpiresIn int    `json:"expires_in"`
}

func (h *WebManagementHandler) PostPair(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("label")
	code, err := h.Pairing.Issue(r.Context(), label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := pairResponse{
		Code:      code,
		ExpiresIn: 300,
	}
	if h.PublicURL != "" {
		resp.URL = h.PublicURL + "/auth?token=" + url.QueryEscape(code)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type deviceResponse struct {
	ID         string  `json:"id"`
	Label      string  `json:"label,omitempty"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt string  `json:"last_seen_at"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

func (h *WebManagementHandler) GetDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.Store.ListDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := make([]deviceResponse, 0, len(devices))
	for _, d := range devices {
		dr := deviceResponse{
			ID:         d.ID,
			Label:      d.Label,
			CreatedAt:  d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastSeenAt: d.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if d.RevokedAt != nil {
			s := d.RevokedAt.UTC().Format("2006-01-02T15:04:05Z")
			dr.RevokedAt = &s
		}
		resp = append(resp, dr)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *WebManagementHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Store.RevokeDevice(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *WebManagementHandler) DeleteAllDevices(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.RevokeAllDevices(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loginPairing redeems a one-time pairing code.
type loginPairing interface {
	Redeem(ctx context.Context, code string) (string, error)
}

// loginSigner issues a session cookie.
type loginSigner interface {
	Issue(w http.ResponseWriter, deviceID string) error
}

// loginDeviceStore persists a new device after successful pairing.
type loginDeviceStore interface {
	InsertDevice(ctx context.Context, id, label string, cookieHash []byte) error
}

// loginRateLimiter guards against brute-force attempts.
type loginRateLimiter interface {
	Allow(ip string) bool
}

// LoginHandler handles /login and /auth.
type LoginHandler struct {
	Pairing loginPairing
	Signer  loginSigner
	Store   loginDeviceStore
	Limiter loginRateLimiter
}

func (h *LoginHandler) GetLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.Login(r.URL.Query().Get("error")).Render(r.Context(), w)
}

func (h *LoginHandler) PostLogin(w http.ResponseWriter, r *http.Request) {
	if !h.Limiter.Allow(remoteIP(r)) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	code := r.FormValue("code")
	label, err := h.Pairing.Redeem(r.Context(), code)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.Login("無効なペアリングコードです").Render(r.Context(), w)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *LoginHandler) GetAuth(w http.ResponseWriter, r *http.Request) {
	if !h.Limiter.Allow(remoteIP(r)) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	token := r.URL.Query().Get("token")
	label, err := h.Pairing.Redeem(r.Context(), token)
	if err != nil {
		http.Redirect(w, r, "/login?error=invalid_code", http.StatusFound)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		http.Redirect(w, r, "/login?error=invalid_code", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// issueSession creates a new device row and sets the session cookie.
func (h *LoginHandler) issueSession(w http.ResponseWriter, r *http.Request, label string) error {
	if h.Signer == nil {
		return fmt.Errorf("session signer not configured")
	}
	deviceID := uuid.New().String()
	sum := sha256.Sum256([]byte(deviceID))
	if err := h.Store.InsertDevice(r.Context(), deviceID, label, sum[:]); err != nil {
		return err
	}
	return h.Signer.Issue(w, deviceID)
}

// remoteIP extracts the host part from r.RemoteAddr.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

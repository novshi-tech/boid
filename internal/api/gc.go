package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type GCAppService struct {
	Store       GCStore
	DeviceStore DeviceGCStore // optional; deletes revoked devices on GC
}

// GC implements orchestrator.GCStore so GCAppService can be passed to GCLoop.
func (s *GCAppService) GC(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error) {
	result, err := s.Store.GC(olderThan, dryRun)
	if err != nil {
		return nil, err
	}
	if s.DeviceStore != nil {
		n, err := s.DeviceStore.DeleteRevokedDevices(context.Background(), dryRun)
		if err != nil {
			slog.Warn("gc devices failed", "error", err)
		} else {
			result.Devices = n
		}
	}
	return result, nil
}

func (s *GCAppService) Run(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error) {
	result, err := s.GC(olderThan, dryRun)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return result, nil
}

type GCHandler struct {
	Service GCService
	// RuntimesDir, when non-empty, is server/wire.go's runtimesDirFor(cfg) —
	// used to list workspace home directories and their sizes in the
	// response (docs/plans/home-workspace-volume.md Phase 4 PR5:
	// "サイズ可視化のみで開始、自動 prune なし"). Left empty, the response
	// omits workspace_homes entirely — no size listing, and (unchanged from
	// pre-PR5) no home directory is ever deleted by GC either way.
	RuntimesDir string
	// Workspaces, when set, is consulted to flag orphaned home directories
	// (a homes/<slug> directory with no corresponding workspace row) in the
	// workspace_homes listing. Optional: a nil Workspaces just means every
	// entry reports orphan=true (ListWorkspaceHomeSizes's degrade-gracefully
	// path — see its doc comment).
	Workspaces WorkspaceSlugLister
}

func (h *GCHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Run)
	return r
}

type gcRequest struct {
	OlderThan string `json:"older_than,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

type gcResponse struct {
	Tasks      int64 `json:"tasks"`
	Jobs       int64 `json:"jobs"`
	Actions    int64 `json:"actions"`
	Runtimes   int64 `json:"runtimes"`
	SandboxTmp int64 `json:"sandbox_tmp"`
	Devices    int64 `json:"devices"`
	DryRun     bool  `json:"dry_run,omitempty"`
	// WorkspaceHomes lists every workspace home directory's on-disk size
	// (docs/plans/home-workspace-volume.md Phase 4 PR5) — visibility only,
	// never auto-pruned by GC. Omitted entirely when GCHandler.RuntimesDir
	// was not wired. Also comes back empty (with WorkspaceHomesListError set)
	// when the workspace lister itself failed — see
	// ListWorkspaceHomeSizes's doc comment (codex PR #791 review,
	// Should-fix #3).
	WorkspaceHomes []WorkspaceHomeSize `json:"workspace_homes,omitempty"`
	// WorkspaceHomesListError is non-empty when WorkspaceSlugLister.List
	// failed while building WorkspaceHomes: orphan detection could not be
	// trusted, so WorkspaceHomes is reported empty instead of every entry
	// silently mismarked Orphan=true, and this field carries the reason.
	WorkspaceHomesListError string `json:"workspace_homes_list_error,omitempty"`
}

func (h *GCHandler) Run(w http.ResponseWriter, r *http.Request) {
	var req gcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	olderThan := 30 * 24 * time.Hour
	if req.OlderThan != "" {
		d, err := time.ParseDuration(req.OlderThan)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid older_than: "+err.Error())
			return
		}
		olderThan = d
	}

	result, err := h.Service.Run(olderThan, req.DryRun)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	resp := gcResponse{
		Tasks:      result.Tasks,
		Jobs:       result.Jobs,
		Actions:    result.Actions,
		Runtimes:   result.Runtimes,
		SandboxTmp: result.SandboxTmp,
		Devices:    result.Devices,
		DryRun:     req.DryRun,
	}
	if h.RuntimesDir != "" {
		homes, listErr, err := ListWorkspaceHomeSizes(h.RuntimesDir, h.Workspaces)
		if err != nil {
			slog.Warn("gc: list workspace homes failed", "error", err)
		} else {
			resp.WorkspaceHomes = homes
			resp.WorkspaceHomesListError = listErr
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

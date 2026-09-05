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
	// Homes, when non-nil, is the engine-backed view of workspace HOME
	// volumes used to list them and their sizes in the response (visibility
	// only, no auto-prune). Left nil, the response omits workspace_homes
	// entirely — no size listing, and no home is ever deleted by GC either
	// way. See WorkspaceHomeStore's doc comment for why the engine handle,
	// rather than a runtimes directory, is the gate.
	Homes WorkspaceHomeStore
	// Workspaces, when set, is consulted to flag orphaned homes (a workspace
	// HOME volume with no corresponding workspace row) in the
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
	// TriggerRuns is the count of finished trigger_runs rows deleted — see
	// orchestrator.GCTriggerRuns.
	TriggerRuns int64 `json:"trigger_runs"`
	// Signals is the count of signals rows deleted — see
	// orchestrator.GCSignals.
	Signals int64 `json:"signals"`
	DryRun  bool  `json:"dry_run,omitempty"`
	// WorkspaceHomes lists every workspace HOME volume's size — visibility
	// only, never auto-pruned by GC. Omitted entirely when GCHandler.Homes
	// was not wired, and empty (with WorkspaceHomesListError set) when the
	// listing could not be produced or trusted.
	WorkspaceHomes []WorkspaceHomeSize `json:"workspace_homes,omitempty"`
	// WorkspaceHomesListError is non-empty when no trustworthy listing could
	// be produced, and carries the reason. Two failures land here: the
	// engine's volume enumeration failed, so there is no listing at all; or
	// WorkspaceSlugLister.List failed, so orphan detection could not be
	// trusted and WorkspaceHomes is reported empty rather than with every
	// entry silently mismarked Orphan=true (see ListWorkspaceHomeSizes's doc
	// comment).
	//
	// One field for both because the CLI's action is the same either way: say
	// the listing is unavailable and why, instead of printing nothing —
	// which, being identical to a genuinely empty install, is what made an
	// enumeration failure invisible.
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
		Tasks:       result.Tasks,
		Jobs:        result.Jobs,
		Actions:     result.Actions,
		Runtimes:    result.Runtimes,
		SandboxTmp:  result.SandboxTmp,
		Devices:     result.Devices,
		TriggerRuns: result.TriggerRuns,
		Signals:     result.Signals,
		DryRun:      req.DryRun,
	}
	if h.Homes != nil {
		homes, listErr, err := ListWorkspaceHomeSizes(r.Context(), h.Homes, h.Workspaces)
		switch {
		case err != nil:
			// The engine could not enumerate the volumes at all. Reported to
			// the caller, not merely logged: the daemon log is not where a
			// `boid gc` operator is looking, and an omitted section with no
			// reason is byte-identical to "this install has no workspace
			// home volumes" — so a wedged engine would read as a clean
			// install.
			//
			// It shares WorkspaceHomesListError with the lister-failure case
			// deliberately: both mean "no trustworthy listing, and here is
			// why", which is the only distinction the CLI acts on. What
			// neither may do is fail the request — GC's own record deletion
			// has already happened and must still be reported.
			slog.Warn("gc: list workspace homes failed", "error", err)
			resp.WorkspaceHomesListError = err.Error()
		default:
			resp.WorkspaceHomes = homes
			resp.WorkspaceHomesListError = listErr
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

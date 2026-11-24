package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// tunnelProjects is the part of api.ProjectAppService the tunnel uses.
type tunnelProjects interface {
	ListProjects(workspaceID string) ([]*orchestrator.Project, error)
	ResolveProjectRef(ref string) ([]*orchestrator.Project, error)
}

// tunnelSessions starts agent jobs (sessionDispatcherAdapter).
type tunnelSessions interface {
	StartSession(ctx context.Context, req api.StartSessionRequest) (*api.StartSessionResult, error)
}

// tunnelJobRequest is the body of POST /jobs over the tunnel.
type tunnelJobRequest struct {
	Project     string `json:"project"`
	Harness     string `json:"harness"`
	Instruction string `json:"instruction"`
	Readonly    bool   `json:"readonly"`
	Model       string `json:"model"`
	Name        string `json:"name"`
}

const maxTunnelInstructionChars = 32000

// tunnelResultContract is appended to every instruction so the job writes
// its result and ends by itself instead of waiting for input.
const tunnelResultContract = `

---
この作業は、ゲートウェイ経由でオーケストレータから頼まれたものです。人は画面の前にいません。
終わったら、結果を次の形の JSON にして /tmp/boid-result.json に書き、` + "`boid job done \"$BOID_JOB_ID\" --output-file /tmp/boid-result.json`" + ` を実行して終えてください。

{"summary": "やったことと結果の要約", "questions": ["判断が要ること(なければ空)"], "artifacts": ["PR の URL など"]}

- 質問があっても待たずに、questions に書いて終える。続きは別の job で頼まれる
- 成果は終える前に push する。このサンドボックスは終わると消える
- 失敗したら summary に原因を書き、` + "`--exit-code 1`" + ` を付ける
- 顧客や個人の情報は要約にとどめ、結果に貼り付けない`

func writeTunnelError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writeTunnelJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// startTunnelJob starts an agent job and answers 202 with its ID.
func startTunnelJob(w http.ResponseWriter, r *http.Request, projects tunnelProjects, sessions tunnelSessions, workspace string) {
	var req tunnelJobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeTunnelError(w, http.StatusBadRequest, `send {"project": "<id or name>", "harness": "claude", "instruction": "..."}`)
		return
	}
	if req.Harness == "" {
		req.Harness = "claude"
	}
	if msg := api.ValidateHarnessType(req.Harness); msg != "" {
		writeTunnelError(w, http.StatusBadRequest, msg)
		return
	}
	req.Instruction = strings.TrimSpace(req.Instruction)
	if req.Instruction == "" {
		writeTunnelError(w, http.StatusBadRequest, "instruction is required")
		return
	}
	if len([]rune(req.Instruction)) > maxTunnelInstructionChars {
		writeTunnelError(w, http.StatusBadRequest, fmt.Sprintf("instruction is longer than %d characters", maxTunnelInstructionChars))
		return
	}
	if req.Project == "" {
		writeTunnelError(w, http.StatusBadRequest, "project is required (GET /projects lists them)")
		return
	}
	all, err := projects.ResolveProjectRef(req.Project)
	if err != nil {
		writeTunnelError(w, statusOf(err, http.StatusNotFound), err.Error())
		return
	}
	var matches []*orchestrator.Project
	for _, p := range all {
		if p.WorkspaceID == workspace {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		writeTunnelError(w, http.StatusNotFound, "no project "+strconv.Quote(req.Project)+" (GET /projects lists them)")
		return
	}
	if len(matches) != 1 {
		names := make([]string, 0, len(matches))
		for _, p := range matches {
			names = append(names, p.ID+" ("+p.Meta.Name+")")
		}
		writeTunnelError(w, http.StatusConflict, "project matches several: "+strings.Join(names, ", "))
		return
	}
	name := req.Name
	if name == "" {
		name = "tunnel: " + req.Harness
	}
	res, err := sessions.StartSession(r.Context(), api.StartSessionRequest{
		ProjectID:   matches[0].ID,
		HarnessType: req.Harness,
		Instruction: req.Instruction + tunnelResultContract,
		Readonly:    req.Readonly,
		Model:       req.Model,
		DisplayName: name,
	})
	if err != nil {
		writeTunnelError(w, statusOf(err, http.StatusInternalServerError), err.Error())
		return
	}
	w.Header().Set("Location", "/jobs/"+res.JobID)
	writeTunnelJSON(w, http.StatusAccepted, map[string]string{
		"id":         res.JobID,
		"status":     "running",
		"result_url": "/jobs/" + res.JobID + "/result",
	})
}

func statusOf(err error, fallback int) int {
	if se, ok := err.(*api.StatusError); ok && se.Code != 0 {
		return se.Code
	}
	return fallback
}

// tunnelJobResult reads a job's result: the JSON the agent passed to
// `boid job done`, or its text when it is not JSON.
func tunnelJobResult(w http.ResponseWriter, r *http.Request, jobs api.JobStore) {
	j, err := jobs.GetJob(chi.URLParam(r, "id"))
	if err != nil {
		writeTunnelError(w, http.StatusNotFound, err.Error())
		return
	}
	out := map[string]any{"id": j.ID, "status": j.Status, "exit_code": j.ExitCode}
	if j.Status != api.JobStatusRunning {
		var parsed any
		if j.Output != "" && json.Unmarshal([]byte(j.Output), &parsed) == nil {
			out["result"] = parsed
		} else if j.Output != "" {
			out["result_text"] = j.Output
		}
	}
	writeTunnelJSON(w, http.StatusOK, out)
}

// listTunnelProjects lists the projects a job can be started in.
func listTunnelProjects(w http.ResponseWriter, _ *http.Request, projects tunnelProjects, workspace string) {
	ps, err := projects.ListProjects(workspace)
	if err != nil {
		writeTunnelError(w, statusOf(err, http.StatusInternalServerError), err.Error())
		return
	}
	type entry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		UpstreamURL string `json:"upstream_url,omitempty"`
	}
	out := make([]entry, 0, len(ps))
	for _, p := range ps {
		if p.WorkspaceID != workspace {
			continue
		}
		out = append(out, entry{ID: p.ID, Name: p.Meta.Name, UpstreamURL: p.UpstreamURL})
	}
	writeTunnelJSON(w, http.StatusOK, out)
}

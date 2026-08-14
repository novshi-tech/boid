package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

// apiGatewayNotifier adapts internal/notify.Service to
// apigateway.UpstreamAuthFailureNotifier — the API gateway's counterpart to
// gatewayNotifier (gitgateway_notify.go), same two-failure-mode distinction
// (docs/plans/api-gateway.md §5 "upstream 401 の notify"):
//
//   - NotifyUpstreamAuthFailure: the upstream service responded with 401 to
//     a request sent WITH injected credentials — the configured secret
//     itself is wrong/expired/revoked.
//   - NotifyCredentialError: credential injection itself failed before the
//     request ever reached the upstream — an unconfigured service, a
//     missing resolver, or a secret-store lookup error.
//
// A nil notify field makes both methods a no-op, matching notify.Service's
// own nil-receiver convention.
type apiGatewayNotifier struct {
	notify *notify.Service
}

// NotifyUpstreamAuthFailure implements apigateway.UpstreamAuthFailureNotifier.
func (n apiGatewayNotifier) NotifyUpstreamAuthFailure(service string) {
	n.send(fmt.Sprintf(
		"api gateway: upstream service %q rejected credentials (401) — the configured secret may be expired or revoked; rotate it with `boid secret set`",
		service))
}

// NotifyCredentialError implements apigateway.UpstreamAuthFailureNotifier.
func (n apiGatewayNotifier) NotifyCredentialError(service string, err error) {
	n.send(fmt.Sprintf(
		"api gateway: could not inject credentials for service %q: %v — check config.yaml's services entry and the referenced secret",
		service, err))
}

func (n apiGatewayNotifier) send(msg string) {
	if n.notify == nil {
		return
	}
	if err := n.notify.Notify(context.Background(), notify.Event{Message: msg}); err != nil {
		slog.Warn("api gateway: notify failed", "error", err)
	}
}

// apiGatewayActionPayload is the JSON shape recorded for one API gateway
// request — docs/plans/api-gateway.md §論点3 ("確定: method + service + path
// + status を timeline に。body は記録しない"). Deliberately narrow: no
// headers, no query string, no request/response body, ever.
type apiGatewayActionPayload struct {
	Method  string `json:"method"`
	Service string `json:"service"`
	Path    string `json:"path"`
	Status  int    `json:"status"`
}

// newAPIGatewayRecorder adapts orchestrator.TaskRepository.CreateAction to
// apigateway.RequestRecorder, so every gateway request past a well-formed-
// route 404 gets appended to its originating task's action log as an
// audit-trail row — docs/plans/api-gateway.md §論点3. taskID == "" (a
// taskless job, e.g. `boid exec`) skips recording entirely: there is no
// task action log to attach it to. A CreateAction failure is logged and
// otherwise swallowed — exactly like gatewayNotifier's own "never abort
// serving other requests" posture — since a lost audit-log row is not a
// reason to fail (or even slow down) the gateway request it describes.
//
// NOTE (2026-08-07, user feedback): these rows are audit-only — deliberately
// EXCLUDED from the rendered task-detail timeline by timeline.Build (see
// ActionTypeAPIGatewayRequest's doc comment in internal/timeline/timeline.go)
// because a task that fans out into a paginated search or a status-polling
// loop was drowning the timeline in one meaningless "api: GET ... → 200"
// row per request. They remain queryable via ListActionsByTask for anyone
// building an audit view.
//
// FromStatus/ToStatus are still both set to the task's CURRENT status (one
// extra GetTask read per request), mirroring api.TaskAppService.NotifyTask's
// own progress-mode Action shape (internal/api/task_notify.go) for
// consistency with other Action rows — even though timeline.Build no longer
// reads it for these rows, it is still meaningful audit context (which
// status the task was in when the request happened). A GetTask failure
// (task deleted mid-request, a transient DB error) is logged and the row is
// skipped entirely rather than written with an empty status.
func newAPIGatewayRecorder(tasks *orchestrator.TaskRepository) apigateway.RequestRecorder {
	return func(req apigateway.RecordedRequest) {
		taskID := req.TaskID
		if taskID == "" || tasks == nil {
			return
		}
		task, err := tasks.GetTask(taskID)
		if err != nil {
			slog.Warn("api gateway: could not resolve task for timeline recording; skipping", "task_id", taskID, "error", err)
			return
		}
		payload, err := json.Marshal(apiGatewayActionPayload{
			Method:  req.Method,
			Service: req.Service,
			Path:    req.Path,
			Status:  req.Status,
		})
		if err != nil {
			slog.Warn("api gateway: encode timeline action payload failed", "task_id", taskID, "error", err)
			return
		}
		action := &orchestrator.Action{
			TaskID:     taskID,
			Type:       timeline.ActionTypeAPIGatewayRequest,
			FromStatus: task.Status,
			ToStatus:   task.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorTask(taskID),
		}
		if err := tasks.CreateAction(action); err != nil {
			slog.Warn("api gateway: record timeline action failed", "task_id", taskID, "error", err)
		}
	}
}

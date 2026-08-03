package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
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

// apiGatewayActionType is the orchestrator.Action.Type recorded for every
// API gateway request — a new, dedicated type (distinct from "progress" or
// any state-transition type) so timeline.BuildActionLabel renders it as its
// own row rather than folding into an existing action category.
const apiGatewayActionType = "api_gateway_request"

// newAPIGatewayRecorder adapts orchestrator.TaskRepository.CreateAction to
// apigateway.RequestRecorder, so every gateway request past a well-formed-
// route 404 gets appended to its originating task's action log (and,
// through the existing timeline builder, its task-detail timeline) —
// docs/plans/api-gateway.md §論点3. taskID == "" (a taskless job, e.g. `boid
// exec`) skips recording entirely: there is no task action log to attach it
// to. A CreateAction failure is logged and otherwise swallowed — exactly
// like gatewayNotifier's own "never abort serving other requests" posture —
// since a lost audit-log row is not a reason to fail (or even slow down) the
// gateway request it describes.
func newAPIGatewayRecorder(tasks *orchestrator.TaskRepository) apigateway.RequestRecorder {
	return func(taskID, method, service, path string, status int) {
		if taskID == "" || tasks == nil {
			return
		}
		payload, err := json.Marshal(apiGatewayActionPayload{
			Method:  method,
			Service: service,
			Path:    path,
			Status:  status,
		})
		if err != nil {
			slog.Warn("api gateway: encode timeline action payload failed", "task_id", taskID, "error", err)
			return
		}
		action := &orchestrator.Action{
			TaskID:  taskID,
			Type:    apiGatewayActionType,
			Payload: payload,
		}
		if err := tasks.CreateAction(action); err != nil {
			slog.Warn("api gateway: record timeline action failed", "task_id", taskID, "error", err)
		}
	}
}

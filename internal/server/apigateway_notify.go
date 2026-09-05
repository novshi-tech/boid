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
// apigateway.UpstreamAuthFailureNotifier, giving upstream 401s and
// credential-injection failures distinct, remediation-oriented messages.
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
// request. Deliberately narrow: no headers, no query string, no request or
// response body, ever.
type apiGatewayActionPayload struct {
	Method  string `json:"method"`
	Service string `json:"service"`
	Path    string `json:"path"`
	Status  int    `json:"status"`
}

// newAPIGatewayRecorder adapts orchestrator.TaskRepository.CreateAction to
// apigateway.RequestRecorder, appending an audit-trail row to a task's
// action log for every gateway request past a well-formed-route 404.
// taskID == "" (a taskless job, e.g. `boid exec`) skips recording entirely.
// A GetTask or CreateAction failure is logged and swallowed rather than
// failing the gateway request it describes.
//
// These rows are audit-only and deliberately excluded from the rendered
// task timeline — see ActionTypeAPIGatewayRequest's doc comment in
// internal/timeline/timeline.go.
func newAPIGatewayRecorder(tasks *orchestrator.TaskRepository) apigateway.RequestRecorder {
	return func(taskID, method, service, path string, status int) {
		if taskID == "" || tasks == nil {
			return
		}
		task, err := tasks.GetTask(taskID)
		if err != nil {
			slog.Warn("api gateway: could not resolve task for timeline recording; skipping", "task_id", taskID, "error", err)
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
			TaskID:     taskID,
			Type:       timeline.ActionTypeAPIGatewayRequest,
			FromStatus: task.Status,
			ToStatus:   task.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorTask(taskID),
		}
		// context.Background(): the RequestRecorder callback carries no
		// ctx of its own, and this always targets the calling task (never
		// a card).
		if err := tasks.CreateAction(context.Background(), action); err != nil {
			slog.Warn("api gateway: record timeline action failed", "task_id", taskID, "error", err)
		}
	}
}

//go:build linux

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// WSAttachHandler handles WebSocket connections for interactive PTY attach.
// Route: GET /api/jobs/{id}/attach/ws
type WSAttachHandler struct {
	Subscriber dispatcher.RuntimeSubscriber
	Writer     dispatcher.RuntimeInputWriter
	PublicURL  string
	Registry   *auth.ConnectionRegistry
}

type wsClientMsg struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"` // base64-encoded for "input" type
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type wsServerMsg struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (h *WSAttachHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: h.allowedOrigins(),
	})
	if err != nil {
		// Accept already wrote the HTTP error response.
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()

	if h.Subscriber == nil {
		h.sendError(ctx, conn, "subscriber not configured")
		conn.Close(websocket.StatusInternalError, "not configured")
		return
	}

	snapshot, ch, cancel, ok := h.Subscriber.Subscribe(jobID)
	defer cancel()

	if len(snapshot) > 0 {
		if err := h.sendOutput(ctx, conn, snapshot); err != nil {
			return
		}
	}

	if !ok || ch == nil {
		h.sendExit(ctx, conn, 0)
		conn.Close(websocket.StatusNormalClosure, "done")
		return
	}

	var revokeCh <-chan struct{}
	if h.Registry != nil {
		if deviceID, ok := auth.DeviceIDFromContext(r.Context()); ok {
			var release func()
			revokeCh, release = h.Registry.Register(deviceID)
			defer release()
		}
	}

	readErrCh := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				readErrCh <- err
				return
			}
			var msg wsClientMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "input":
				data, err := base64.StdEncoding.DecodeString(msg.Data)
				if err != nil {
					continue
				}
				if h.Writer != nil {
					h.Writer.WriteInput(jobID, data) //nolint:errcheck
				}
			case "resize":
				if h.Writer != nil {
					h.Writer.ResizeRuntime(jobID, dispatcher.TerminalSize{Cols: msg.Cols, Rows: msg.Rows}) //nolint:errcheck
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-readErrCh:
			return
		case chunk, more := <-ch:
			if !more {
				h.sendExit(ctx, conn, 0)
				conn.Close(websocket.StatusNormalClosure, "process exited")
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if err := h.sendOutput(ctx, conn, chunk); err != nil {
				return
			}
		case <-revokeCh:
			conn.Close(websocket.StatusNormalClosure, "revoked")
			return
		}
	}
}

func (h *WSAttachHandler) allowedOrigins() []string {
	patterns := []string{"localhost", "127.0.0.1", "[::1]"}
	if h.PublicURL != "" {
		if u, err := url.Parse(h.PublicURL); err == nil && u.Host != "" {
			patterns = append(patterns, u.Host)
		}
	}
	return patterns
}

func (h *WSAttachHandler) sendOutput(ctx context.Context, conn *websocket.Conn, data []byte) error {
	msg := wsServerMsg{Type: "output", Data: base64.StdEncoding.EncodeToString(data)}
	b, _ := json.Marshal(msg)
	return conn.Write(ctx, websocket.MessageText, b)
}

func (h *WSAttachHandler) sendExit(ctx context.Context, conn *websocket.Conn, code int) {
	msg := wsServerMsg{Type: "exit", Code: code}
	b, _ := json.Marshal(msg)
	conn.Write(ctx, websocket.MessageText, b) //nolint:errcheck
}

func (h *WSAttachHandler) sendError(ctx context.Context, conn *websocket.Conn, message string) {
	msg := wsServerMsg{Type: "error", Message: message}
	b, _ := json.Marshal(msg)
	conn.Write(ctx, websocket.MessageText, b) //nolint:errcheck
}

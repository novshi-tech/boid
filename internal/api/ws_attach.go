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

	// Bearer verifies an `Authorization: Bearer <token>` header carried on
	// the WS handshake request (docs/plans/cli-remote-connection.md Phase 3
	// PR0). When present, it is checked before auth.DeviceIDFromContext —
	// see authenticateDevice's doc comment for the precedence rule. PR3
	// moved this handler's mount point in internal/server/wire.go out of
	// the cookie-only WebAuthMiddleware Group so a Bearer-only caller (the
	// CLI's WS-based AttachJob, internal/client/client.go) can actually
	// reach this route end-to-end over TCP; the field itself has existed
	// since PR0.
	Bearer *auth.BearerVerifier
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

	deviceID, deviceOK, authErr := h.authenticateDevice(r)
	if authErr != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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
	if h.Registry != nil && deviceOK {
		var release func()
		revokeCh, release = h.Registry.Register(deviceID)
		defer release()
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
			case "input_close":
				// The client's own stdin hit EOF (or it never had one) —
				// propagate that to the job's process so a pipe-oriented
				// non-interactive command (`cat`, `wc`, ...) sees a real
				// EOF and can exit (docs/plans/cli-remote-connection.md
				// Phase 3 PR3; see LocalRuntime.CloseInputRuntime's doc
				// comment). No-op for interactive PTY sessions and for
				// non-interactive sessions with no StdinForward pipe.
				if h.Writer != nil {
					h.Writer.CloseInput(jobID) //nolint:errcheck
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
				// The exit frame's code is placeholder-0 today: the actual
				// exit code is surfaced via a separate REST endpoint
				// (cmd/exec.go's fetchExecExitCode → GET /api/jobs/{id}/exit-code),
				// not through this WS frame, and there is no path from
				// the runtime subscriber's chunk channel to the process
				// exit code here. Rewiring exit-code propagation to run
				// through this frame is the Phase 3 未解決論点 the plan
				// doc tracks; the frame type stays reserved for the day
				// we do that. See client.go's attachReadOutput's "exit"
				// case for the mirror on the reader side.
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

// authenticateDevice resolves the caller's device ID for the WS handshake
// request r, before any websocket.Accept happens (so a rejection is a plain
// HTTP 401, not a WS close frame). An Authorization: Bearer header, when
// present, is verified via h.Bearer and takes priority — the same
// precedence auth.NewTCPAPIAuthMiddleware uses (Phase 3 PR0: Bearer is a
// hard commitment, no falling back to the context-derived ID on failure).
// Without a Bearer header this falls back to
// auth.DeviceIDFromContext(r.Context()) — the device ID set by whatever
// cookie-based middleware sits in front of this handler in the router,
// unchanged from before PR0.
func (h *WSAttachHandler) authenticateDevice(r *http.Request) (deviceID string, ok bool, err error) {
	if _, present, _ := auth.ExtractBearerToken(r); present {
		if h.Bearer == nil {
			return "", false, auth.ErrInvalidSession
		}
		id, verifyErr := h.Bearer.Verify(r)
		if verifyErr != nil {
			return "", false, verifyErr
		}
		return id, true, nil
	}
	id, present := auth.DeviceIDFromContext(r.Context())
	return id, present, nil
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

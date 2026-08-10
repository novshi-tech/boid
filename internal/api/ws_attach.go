//go:build linux

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

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

	// KeepalivePeriod overrides how often an idle connection is pinged.
	// Zero means defaultWSKeepalivePeriod; tests set it short.
	KeepalivePeriod time.Duration
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
	// Offset is meaningful only on the "attach" frame: the byte position
	// within the job's accumulated transcript that the replay about to
	// follow starts at. See sendAttach's doc comment.
	Offset int `json:"offset,omitempty"`
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

	snapshot, ch, cancel, ok, finished := h.Subscriber.Subscribe(jobID)
	defer cancel()

	replayFrom := clampReplayOffset(replayOffsetFromRequest(r), len(snapshot))
	if err := h.sendAttach(ctx, conn, replayFrom); err != nil {
		return
	}
	if replayFrom < len(snapshot) {
		if err := h.sendOutput(ctx, conn, snapshot[replayFrom:]); err != nil {
			return
		}
	}

	if !ok || ch == nil {
		if !finished {
			// The job is still genuinely running but has no live stream
			// to offer right now (Opus review of PR #864, B2) — e.g. the
			// container backend's adopt-time attach failed and hasn't
			// been re-attached yet. Reporting exit 0 here (this
			// branch's behavior before B2) would be a false positive:
			// the caller would be told a still-running job finished
			// successfully, which is a worse diagnostic than the
			// dead-channel hang this whole fix started from — at least
			// that hang was an honest "something is wrong" signal. An
			// error frame lets `boid job attach` (internal/client's
			// attachReadOutput already returns a real error for a
			// "error"-typed frame) and any other WS caller retry instead
			// of believing the job is over.
			h.sendError(ctx, conn, "job is still running but has no live output stream right now; try again")
			conn.Close(websocket.StatusNormalClosure, "stream unavailable")
			return
		}
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

	keepalive := time.NewTicker(h.keepalivePeriod())
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-readErrCh:
			return
		case <-keepalive.C:
			// A PTY that is merely idle (an agent waiting on the user)
			// sends no bytes at all, and every intermediary between the
			// CLI and this handler — Cloudflare Tunnel most notably, but
			// also plain NAT/conntrack — eventually reaps a connection
			// with no traffic on it. Nothing in boid times an attach out
			// (this handler's http.Server sets no Read/Write/Idle
			// timeout, and coder/websocket sends no ping of its own), so
			// an idle-drop was purely the network's doing and looked to
			// the user like boid hanging up. A ping is real traffic and
			// keeps those timers from firing; it also makes a genuinely
			// dead peer surface here as a write error instead of leaving
			// this goroutine parked on a channel forever.
			pingCtx, cancelPing := context.WithTimeout(ctx, wsPingTimeout)
			err := conn.Ping(pingCtx)
			cancelPing()
			if err != nil {
				return
			}
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
//
// PR-3 Option 4 round-2 codex review Blocker 1: a request that already
// authenticated via the dedicated CLI TCP listener
// (auth.NewCLITokenAuthMiddleware, marked via
// auth.WithCLITokenAuthenticated) must short-circuit here BEFORE the Bearer
// branch below — that listener's Bearer header carries BOID_CLI_TOKEN, a
// single shared secret never written to the auth store as a device token,
// so handing it to h.Bearer.Verify would always fail with 401 even though
// the request is already fully authenticated (this is exactly how `boid
// attach`/`exec` broke over host mode: the CLI middleware accepted the
// token, then this handler re-checked the same header and rejected it).
// No device ID to report either way — CLI-token auth carries no
// paired-device identity to register with h.Registry for revocation.
func (h *WSAttachHandler) authenticateDevice(r *http.Request) (deviceID string, ok bool, err error) {
	if auth.CLITokenAuthenticated(r.Context()) {
		return "", false, nil
	}
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

// maxOutputChunkBytes is the largest RAW payload one "output" frame
// carries. It exists because the transcript this handler replays on
// connect (dispatcher's containerSession.Subscribe returns the whole
// accumulated transcript, which is unbounded) used to go out as a single
// frame — and coder/websocket's DEFAULT read limit on the receiving side is
// 32768 bytes, so attaching to any job that had printed more than ~24 KB
// died instantly with "websocket: message too big: read limited at 32769
// bytes" (reported from `boid attach` on Windows). 16 KiB raw base64-
// inflates to 21848 bytes, which plus the JSON envelope stays comfortably
// under 32768 — i.e. an OLD client with the stock read limit can still
// attach to a NEW daemon. Browsers impose no such limit, which is why the
// Web UI terminal never hit this.
const maxOutputChunkBytes = 16 * 1024

func (h *WSAttachHandler) sendOutput(ctx context.Context, conn *websocket.Conn, data []byte) error {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxOutputChunkBytes {
			chunk = chunk[:maxOutputChunkBytes]
		}
		data = data[len(chunk):]
		msg := wsServerMsg{Type: "output", Data: base64.StdEncoding.EncodeToString(chunk)}
		b, _ := json.Marshal(msg)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			return err
		}
	}
	return nil
}

// defaultWSKeepalivePeriod is how often the send loop pings an otherwise
// silent attach connection, and wsPingTimeout bounds how long one such ping
// waits for its pong before the connection is declared dead.
//
// 30s is chosen against the shortest idle window boid actually runs behind
// in practice — Cloudflare's ~100s proxy idle timeout (docs/ja/guide/
// web-ui.md's recommended Tunnel setup) — with enough margin that a single
// dropped ping does not cost the connection.
const (
	defaultWSKeepalivePeriod = 30 * time.Second
	wsPingTimeout            = 10 * time.Second
)

func (h *WSAttachHandler) keepalivePeriod() time.Duration {
	if h.KeepalivePeriod > 0 {
		return h.KeepalivePeriod
	}
	return defaultWSKeepalivePeriod
}

// replayOffsetFromRequest reads the ?replay_offset=<bytes> query parameter
// a RECONNECTING client sends to say how much of this job's transcript it
// already has. Absent, malformed, or negative values mean "replay
// everything", which is exactly what a first-time attach wants — so an old
// client that has never heard of this parameter keeps its old behavior.
func replayOffsetFromRequest(r *http.Request) int {
	raw := r.URL.Query().Get("replay_offset")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// clampReplayOffset resolves a client-claimed offset against the snapshot
// actually on hand. An offset past the end of the snapshot is NOT an error
// to reject: the daemon may have restarted and rebuilt a shorter (or empty)
// transcript for the same job, in which case the honest answer is to replay
// from the beginning rather than send the client nothing and leave it
// staring at a blank screen. The client learns which of the two happened
// from the "attach" frame's own offset field.
func clampReplayOffset(offset, snapshotLen int) int {
	if offset <= 0 || offset > snapshotLen {
		return 0
	}
	return offset
}

// sendAttach announces, as the first frame of every connection, where the
// replay that follows starts within the job's transcript. A client tracking
// its own byte count can add this to the payload bytes it receives and hand
// the total back as ?replay_offset on its next reconnect, so a reconnect
// resumes mid-transcript instead of repainting the whole session.
//
// Clients that predate this frame ignore an unknown frame type (both the
// CLI's attachReadOutput and the Web UI's onmessage fall through unknown
// types), so sending it unconditionally is backward compatible.
func (h *WSAttachHandler) sendAttach(ctx context.Context, conn *websocket.Conn, offset int) error {
	msg := wsServerMsg{Type: "attach", Offset: offset}
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

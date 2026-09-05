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
	"github.com/novshi-tech/boid/internal/vtsnapshot"
)

// WSAttachHandler handles WebSocket connections for interactive PTY attach.
// Route: GET /api/jobs/{id}/attach/ws
type WSAttachHandler struct {
	Subscriber dispatcher.RuntimeSubscriber
	Writer     dispatcher.RuntimeInputWriter
	PublicURL  string
	Registry   *auth.ConnectionRegistry

	// Bearer verifies an `Authorization: Bearer <token>` header carried on
	// the WS handshake request. When present, it is checked before
	// auth.DeviceIDFromContext — see authenticateDevice's doc comment for
	// the precedence rule. This handler's mount point (internal/server/
	// wire.go) sits outside the cookie-only WebAuthMiddleware Group so a
	// Bearer-only caller (the CLI's WS-based AttachJob) can reach this route
	// end-to-end over TCP.
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
	// Rendered is meaningful only on the "attach" frame: true means the
	// replay that follows is a resolved screen dump rather than a slice of
	// the raw transcript, so the client must clear its terminal before
	// applying it (a screen dump spliced onto whatever was already there
	// would double the visible content). See sendAttach.
	Rendered bool `json:"rendered,omitempty"`
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

	replay, replayFrom, rendered := resolveReplay(snapshot, replayOffsetFromRequest(r))
	if err := h.sendAttach(ctx, conn, replayFrom, rendered); err != nil {
		return
	}
	if err := h.sendOutput(ctx, conn, replay); err != nil {
		return
	}

	if !ok || ch == nil {
		if !finished {
			// The job is still genuinely running but has no live stream to
			// offer right now (e.g. the container backend's adopt-time
			// attach failed and hasn't been re-attached yet). Reporting
			// exit 0 here would be a false positive: the caller would be
			// told a still-running job finished successfully. An error
			// frame lets `boid job attach` and any other WS caller retry
			// instead of believing the job is over.
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
				// EOF and can exit. No-op for interactive PTY sessions and
				// for non-interactive sessions with no StdinForward pipe.
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
				// (cmd/exec.go's fetchExecExitCode → GET
				// /api/jobs/{id}/exit-code), not through this WS frame —
				// there is no path from the runtime subscriber's chunk
				// channel to the process exit code here. See client.go's
				// attachReadOutput's "exit" case for the mirror on the
				// reader side.
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
// present, is verified via h.Bearer and takes priority (a hard commitment,
// no falling back to the context-derived ID on failure) — the same
// precedence auth.NewTCPAPIAuthMiddleware uses. Without a Bearer header this
// falls back to auth.DeviceIDFromContext(r.Context()) — the device ID set by
// whatever cookie-based middleware sits in front of this handler in the
// router.
//
// A request that already authenticated via the dedicated CLI TCP listener
// (auth.NewCLITokenAuthMiddleware, marked via auth.WithCLITokenAuthenticated)
// must short-circuit here BEFORE the Bearer branch below — that listener's
// Bearer header carries BOID_CLI_TOKEN, a single shared secret never
// written to the auth store as a device token, so handing it to
// h.Bearer.Verify would always fail with 401 even though the request is
// already fully authenticated. No device ID to report either way —
// CLI-token auth carries no paired-device identity to register with
// h.Registry for revocation.
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

// resolveReplay decides what a connecting client actually receives, and
// returns it alongside the transcript position it leaves the client at and
// whether it is a rendered screen rather than raw bytes.
//
// Two cases, and the distinction is the whole point:
//
//   - A RECONNECT (a usable ?replay_offset) gets the raw tail it missed.
//     That is a small, exact splice onto a screen the client still has, and
//     it keeps the transcript byte accounting trivially correct.
//   - A FRESH connect to a PTY session gets the transcript resolved to the
//     screen it painted, because replaying it raw is what made the Web UI
//     terminal scroll the entire session past on every connect: a
//     full-screen TUI overdraws the same cells for the whole session (8.7 MB
//     and 39699 frames on one measured job, to describe one 80x60 screen).
//     The resolved dump is ~1000x smaller and, unlike a naive byte-count
//     bound on the raw stream, actually correct — see internal/vtsnapshot.
//
// A fresh connect to a non-PTY session (a hook job's log, `boid exec` on the
// non-interactive transport) is NOT a screen recording and keeps replaying
// verbatim.
//
// The returned offset is always a position in the RAW transcript, including
// in the rendered case, so the client's own byte counting — and the
// ?replay_offset it hands back on its next reconnect — stays anchored to the
// one thing both ends agree on, regardless of what was actually painted.
func resolveReplay(snapshot dispatcher.RuntimeSnapshot, requestedOffset int) (replay []byte, offset int, rendered bool) {
	from := clampReplayOffset(requestedOffset, len(snapshot.Raw))
	if from > 0 {
		return snapshot.Raw[from:], from, false
	}
	if !snapshot.TTY || len(snapshot.Raw) == 0 {
		return snapshot.Raw, 0, false
	}
	return vtsnapshot.Render(snapshot.Raw, snapshot.Geometry.Cols, snapshot.Geometry.Rows), len(snapshot.Raw), true
}

// sendAttach announces, as the first frame of every connection, where the
// replay that follows starts within the job's transcript and whether that
// replay is a rendered screen. A client tracking its own byte count can add
// the offset to the payload bytes it receives and hand the total back as
// ?replay_offset on its next reconnect, so a reconnect resumes mid-transcript
// instead of repainting the whole session.
//
// Clients that predate this frame ignore an unknown frame type (both the
// CLI's attachReadOutput and the Web UI's onmessage fall through unknown
// types), so sending it unconditionally is backward compatible. A client that
// knows the frame but not its rendered field is a narrower case worth being
// explicit about: it reads offset correctly and simply misses the "clear
// first" instruction, so it paints a correct screen dump onto a terminal it
// did not clear. That is a cosmetic duplication on first connect, not a
// desync — offset accounting is unaffected — and it self-corrects on the
// job's next full repaint.
func (h *WSAttachHandler) sendAttach(ctx context.Context, conn *websocket.Conn, offset int, rendered bool) error {
	msg := wsServerMsg{Type: "attach", Offset: offset, Rendered: rendered}
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

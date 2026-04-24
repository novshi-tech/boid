// Package websocket is a local stub for github.com/coder/websocket.
// It provides a minimal RFC 6455 implementation sufficient for
// internal/api/ws_attach.go and its tests.
// Only the API surface used by this project is implemented.
package websocket

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// MessageType mirrors coder/websocket.MessageType.
type MessageType int

const (
	MessageText   MessageType = 1
	MessageBinary MessageType = 2
)

// StatusCode mirrors coder/websocket.StatusCode.
type StatusCode int

const (
	StatusNormalClosure StatusCode = 1000
	StatusInternalError StatusCode = 1011
)

// AcceptOptions mirrors coder/websocket.AcceptOptions.
type AcceptOptions struct {
	OriginPatterns []string
}

// DialOptions mirrors coder/websocket.DialOptions.
type DialOptions struct {
	HTTPHeader http.Header
}

// Conn is a WebSocket connection.
type Conn struct {
	conn   net.Conn
	reader io.Reader // may be bufio.Reader wrapping conn; used for reads
	mu     sync.Mutex
	// server is true when this side should NOT mask frames (server→client).
	server bool
}

// --- server-side Accept ---

// Accept upgrades the HTTP connection to a WebSocket connection.
func Accept(w http.ResponseWriter, r *http.Request, opts *AcceptOptions) (*Conn, error) {
	if opts != nil && len(opts.OriginPatterns) > 0 {
		origin := r.Header.Get("Origin")
		if !originAllowed(origin, opts.OriginPatterns) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return nil, fmt.Errorf("websocket: forbidden origin %q", origin)
		}
	}

	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, fmt.Errorf("websocket: missing key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("websocket: hijack not supported")
	}

	nc, buf, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("websocket: hijack: %w", err)
	}

	accept := computeAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buf.WriteString(resp); err != nil {
		nc.Close()
		return nil, fmt.Errorf("websocket: write handshake: %w", err)
	}
	if err := buf.Flush(); err != nil {
		nc.Close()
		return nil, fmt.Errorf("websocket: flush handshake: %w", err)
	}

	return &Conn{conn: nc, reader: buf.Reader, server: true}, nil
}

func originAllowed(origin string, patterns []string) bool {
	if origin == "" {
		return true
	}
	host := origin
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	// strip port
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, p := range patterns {
		if p == host || p == "localhost" && (host == "127.0.0.1" || host == "::1") {
			return true
		}
		if p == host {
			return true
		}
	}
	return false
}

func computeAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// --- client-side Dial ---

// Dial connects to a WebSocket server.
func Dial(ctx context.Context, rawURL string, opts *DialOptions) (*Conn, *http.Response, error) {
	// Convert ws:// → http:// and wss:// → https:// for the dial address.
	addr := rawURL
	scheme := "tcp"
	if strings.HasPrefix(rawURL, "wss://") {
		addr = "https://" + rawURL[6:]
		scheme = "tls"
	} else if strings.HasPrefix(rawURL, "ws://") {
		addr = rawURL[5:] // host:port/path...
	} else {
		return nil, nil, fmt.Errorf("websocket: unsupported URL scheme: %s", rawURL)
	}

	// Extract host and path.
	host := addr
	path := "/"
	if idx := strings.Index(addr, "/"); idx >= 0 {
		host = addr[:idx]
		path = addr[idx:]
	}

	if scheme == "tls" {
		return nil, nil, fmt.Errorf("websocket: TLS not supported in stub")
	}

	var dialer net.Dialer
	nc, err := dialer.DialContext(ctx, "tcp", ensurePort(host, "80"))
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: dial: %w", err)
	}

	// Generate Sec-WebSocket-Key.
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("websocket: random key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	// Build HTTP upgrade request.
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+host+path, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if opts != nil {
		for k, vs := range opts.HTTPHeader {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	req.Host = host

	if err := req.Write(nc); err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("websocket: write request: %w", err)
	}

	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("websocket: read response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		nc.Close()
		return nil, resp, fmt.Errorf("websocket: server rejected upgrade: %s", resp.Status)
	}

	// Verify Sec-WebSocket-Accept.
	expected := computeAccept(key)
	got := resp.Header.Get("Sec-WebSocket-Accept")
	if got != expected {
		nc.Close()
		return nil, resp, fmt.Errorf("websocket: bad Sec-WebSocket-Accept: %q", got)
	}

	// Use br for reads: it may have buffered WebSocket frame bytes that arrived
	// together with the 101 response before we could read them from nc directly.
	return &Conn{conn: nc, reader: br, server: false}, resp, nil
}

func ensurePort(host, defaultPort string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host // already has port
	}
	return host + ":" + defaultPort
}

// --- frame I/O ---

const (
	opText  = 0x1
	opBin   = 0x2
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// Read reads the next message from the connection.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	type result struct {
		mt  MessageType
		buf []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		mt, buf, err := c.readFrame()
		ch <- result{mt, buf, err}
	}()
	select {
	case <-ctx.Done():
		c.conn.Close()
		return 0, nil, ctx.Err()
	case r := <-ch:
		return r.mt, r.buf, r.err
	}
}

func (c *Conn) readFrame() (MessageType, []byte, error) {
	r := c.reader
	// Read first two bytes.
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, fmt.Errorf("ws read header: %w", err)
	}
	// fin := (header[0] & 0x80) != 0  // not used in stub (assumes single-frame messages)
	opcode := header[0] & 0x0F
	masked := (header[1] & 0x80) != 0
	payLen := int64(header[1] & 0x7F)

	switch payLen {
	case 126:
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return 0, nil, err
		}
		payLen = int64(n)
	case 127:
		if err := binary.Read(r, binary.BigEndian, &payLen); err != nil {
			return 0, nil, err
		}
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, payLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	switch opcode {
	case opClose:
		return 0, nil, io.EOF
	case opPing:
		// respond with pong (best-effort)
		c.Write(context.Background(), MessageText, payload) //nolint:errcheck
		return c.readFrame()
	case opText:
		return MessageText, payload, nil
	default:
		return MessageBinary, payload, nil
	}
}

// Write sends a message to the connection.
func (c *Conn) Write(ctx context.Context, mt MessageType, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	opcode := byte(opText)
	if mt == MessageBinary {
		opcode = opBin
	}

	frame := buildFrame(opcode, data, !c.server)
	if _, err := c.conn.Write(frame); err != nil {
		return fmt.Errorf("ws write: %w", err)
	}
	return nil
}

func buildFrame(opcode byte, data []byte, mask bool) []byte {
	payLen := len(data)
	var header []byte

	b0 := byte(0x80) | opcode // FIN + opcode
	header = append(header, b0)

	var maskBit byte
	if mask {
		maskBit = 0x80
	}

	switch {
	case payLen <= 125:
		header = append(header, maskBit|byte(payLen))
	case payLen <= 65535:
		header = append(header, maskBit|126)
		header = append(header, byte(payLen>>8), byte(payLen))
	default:
		header = append(header, maskBit|127)
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(payLen))
		header = append(header, buf...)
	}

	if mask {
		var key [4]byte
		rand.Read(key[:]) //nolint:errcheck
		header = append(header, key[:]...)
		masked := make([]byte, payLen)
		for i, b := range data {
			masked[i] = b ^ key[i%4]
		}
		return append(header, masked...)
	}
	return append(header, data...)
}

// Close sends a close frame with the given status code and reason.
func (c *Conn) Close(code StatusCode, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	frame := buildFrame(opClose, payload, !c.server)
	c.conn.Write(frame) //nolint:errcheck
	return c.conn.Close()
}

// CloseNow closes the connection without sending a close frame.
func (c *Conn) CloseNow() error {
	return c.conn.Close()
}

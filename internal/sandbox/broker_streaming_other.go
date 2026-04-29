//go:build !linux

package sandbox

import "net"

// handleStreamingExec on non-Linux platforms falls back to the non-streaming
// Handle path and converts the result to stream format. PTY-based line
// buffering and signal propagation are Linux-only features.
func (b *Broker) handleStreamingExec(conn net.Conn, req *ExecRequest) {
	resp := b.Handle(req)
	sendStreamResponse(conn, resp)
}

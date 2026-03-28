package sandbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

type Proxy struct {
	allowedDomains []string
	listener       net.Listener
	server         *http.Server
	mu             sync.Mutex
}

func NewProxy(allowedDomains []string) *Proxy {
	return &Proxy{allowedDomains: allowedDomains}
}

func (p *Proxy) Start(ctx context.Context) (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen: %w", err)
	}
	p.listener = ln

	p.server = &http.Server{
		Handler: http.HandlerFunc(p.handleConnect),
	}

	go p.server.Serve(ln)
	go func() {
		<-ctx.Done()
		p.Stop()
	}()

	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (p *Proxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		p.server.Close()
	}
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if !p.isDomainAllowed(host) {
		http.Error(w, "domain not allowed", http.StatusForbidden)
		return
	}

	targetConn, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		targetConn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		targetConn.Close()
		return
	}

	go io.Copy(targetConn, clientConn)
	go io.Copy(clientConn, targetConn)
}

func (p *Proxy) isDomainAllowed(domain string) bool {
	domain = strings.ToLower(domain)
	for _, d := range p.allowedDomains {
		if strings.ToLower(d) == domain {
			return true
		}
	}
	return false
}

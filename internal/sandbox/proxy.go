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
		Handler: http.HandlerFunc(p.serveHTTP),
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

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
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

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}

	host := r.URL.Hostname()
	if !p.isDomainAllowed(host) {
		http.Error(w, "domain not allowed", http.StatusForbidden)
		return
	}

	r.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// isDomainAllowed checks if a domain matches the allowed list.
// Entries starting with "." match as a suffix (e.g. ".docker.io" matches "registry-1.docker.io").
func (p *Proxy) isDomainAllowed(domain string) bool {
	domain = strings.ToLower(domain)
	for _, d := range p.allowedDomains {
		d = strings.ToLower(d)
		if strings.HasPrefix(d, ".") {
			if strings.HasSuffix(domain, d) || domain == d[1:] {
				return true
			}
		} else if d == domain {
			return true
		}
	}
	return false
}

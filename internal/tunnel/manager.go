package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Manager owns the daemon's tunnel: the identity file, the running client
// and an in-progress pairing.
type Manager struct {
	IdentityPath string
	Handler      http.Handler
	Description  string
	Version      string
	HTTPClient   *http.Client

	mu        sync.Mutex
	ctx       context.Context
	client    *Client
	cancel    context.CancelFunc
	pairing   *Pairing
	pairError string
	pairStop  context.CancelFunc
}

// ManagerStatus is what GET /api/tunnel returns.
type ManagerStatus struct {
	Paired     bool     `json:"paired"`
	Connection *Status  `json:"connection,omitempty"`
	Pairing    *Pairing `json:"pairing,omitempty"`
	PairError  string   `json:"pair_error,omitempty"`
}

// Start connects with the saved identity, if any, until ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	id, err := LoadIdentity(m.IdentityPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	m.run(id)
	return nil
}

func (m *Manager) run(id *Identity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	c := &Client{Identity: id, Handler: m.Handler, Description: m.Description, Version: m.Version, HTTPClient: m.HTTPClient}
	m.client, m.cancel = c, cancel
	go func() { _ = c.Run(ctx) }()
}

// Caller returns the connected client, or nil while unpaired.
func (m *Manager) Caller() Caller {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == nil {
		return nil
	}
	return m.client
}

// Status reports pairing and connection state.
func (m *Manager) Status() ManagerStatus {
	m.mu.Lock()
	client, pairing, pairErr := m.client, m.pairing, m.pairError
	m.mu.Unlock()
	out := ManagerStatus{Paired: client != nil, Pairing: pairing, PairError: pairErr}
	if client != nil {
		s := client.Status()
		out.Connection = &s
	}
	return out
}

var gatewayURL = regexp.MustCompile(`^https?://[^/]+$`)

// Pair starts pairing with gateway; approval completes in the background
// and replaces any previous identity.
func (m *Manager) Pair(gateway, name string) (*Pairing, error) {
	gateway = strings.TrimRight(gateway, "/")
	if u, err := url.Parse(gateway); err != nil || !gatewayURL.MatchString(gateway) || (u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		return nil, errors.New("gateway must be an https:// origin such as https://gateway.example.com")
	}
	if name == "" {
		name = "boid"
	}
	m.mu.Lock()
	ctx := m.ctx
	if m.pairStop != nil {
		m.pairStop()
	}
	m.mu.Unlock()
	if ctx == nil {
		return nil, errors.New("tunnel manager is not started")
	}
	hc := m.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	p, err := StartPairing(ctx, hc, gateway, name, m.Description)
	if err != nil {
		return nil, err
	}
	pairCtx, stop := context.WithDeadline(ctx, p.ExpiresAt)
	m.mu.Lock()
	m.pairing, m.pairError, m.pairStop = p, "", stop
	m.mu.Unlock()
	go func() {
		defer stop()
		id, err := p.Wait(pairCtx, hc)
		if err == nil {
			err = SaveIdentity(m.IdentityPath, id)
		}
		if err == nil {
			slog.Info("tunnel: paired", "gateway", gateway, "name", id.Name)
			m.run(id)
		} else {
			slog.Warn("tunnel: pairing did not complete", "gateway", gateway, "err", err)
		}
		m.mu.Lock()
		if m.pairing == p {
			m.pairing = nil
			if err != nil {
				m.pairError = err.Error()
			}
		}
		m.mu.Unlock()
	}()
	return p, nil
}

// Forget disconnects and deletes the saved identity. The registration at the
// gateway stays until the owner removes it there.
func (m *Manager) Forget() error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	if m.pairStop != nil {
		m.pairStop()
	}
	m.client, m.cancel, m.pairing, m.pairStop, m.pairError = nil, nil, nil, nil, ""
	m.mu.Unlock()
	if err := os.Remove(m.IdentityPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// MountPath is where the daemon serves the management API.
const MountPath = "/api/tunnel"

// ServeHTTP serves the management API under MountPath.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, MountPath), "/")
	switch {
	case tail == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, m.Status())
	case tail == "" && r.Method == http.MethodDelete:
		if err := m.Forget(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case tail == "/pair" && r.Method == http.MethodPost:
		var in struct {
			Gateway string `json:"gateway"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"gateway": "https://..."}`})
			return
		}
		p, err := m.Pair(in.Gateway, in.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, p)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

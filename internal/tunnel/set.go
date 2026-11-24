package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// DefaultWorkspace inherits the identity paired before tunnels were per workspace.
const DefaultWorkspace = "default"

// MountPath is where the daemon serves the management API.
const MountPath = "/api/tunnel"

var workspaceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Set keeps one tunnel per workspace, each paired on its own and reaching
// only its own workspace, so one organisation's gateway never sees another's.
type Set struct {
	// Dir holds <workspace>.json identity files.
	Dir string
	// LegacyPath is the single identity file of earlier releases; Start moves
	// it to DefaultWorkspace.
	LegacyPath string
	// Handler serves one workspace's owner requests.
	Handler func(workspace string) http.Handler
	// Description is the hello description for one workspace.
	Description func(workspace string) string
	// CheckWorkspace rejects pairing a workspace that does not exist.
	CheckWorkspace func(workspace string) error
	Version        string
	HTTPClient     *http.Client

	mu       sync.Mutex
	ctx      context.Context
	managers map[string]*Manager
}

// WorkspaceStatus is one workspace's entry in GET /api/tunnel.
type WorkspaceStatus struct {
	Workspace string `json:"workspace"`
	ManagerStatus
}

// Start connects every paired workspace until ctx ends.
func (s *Set) Start(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	if err := s.adoptLegacy(); err != nil {
		slog.Warn("tunnel: could not move the old identity to the default workspace", "err", err)
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		ws, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !workspaceName.MatchString(ws) {
			continue
		}
		if _, err := s.manager(ws); err != nil {
			slog.Warn("tunnel: not started", "workspace", ws, "err", err)
		}
	}
	return nil
}

func (s *Set) adoptLegacy() error {
	if s.LegacyPath == "" {
		return nil
	}
	if _, err := os.Stat(s.LegacyPath); err != nil {
		return nil
	}
	target := s.identityPath(DefaultWorkspace)
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return os.Rename(s.LegacyPath, target)
}

func (s *Set) identityPath(ws string) string { return filepath.Join(s.Dir, ws+".json") }

// manager returns the workspace's tunnel, starting it on first use.
func (s *Set) manager(ws string) (*Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.managers[ws]; ok {
		return m, nil
	}
	if s.ctx == nil {
		return nil, errors.New("tunnel is not started")
	}
	m := &Manager{IdentityPath: s.identityPath(ws), Version: s.Version, HTTPClient: s.HTTPClient}
	if s.Handler != nil {
		m.Handler = s.Handler(ws)
	}
	if s.Description != nil {
		m.Description = s.Description(ws)
	}
	if err := m.Start(s.ctx); err != nil {
		return nil, err
	}
	if s.managers == nil {
		s.managers = map[string]*Manager{}
	}
	s.managers[ws] = m
	return m, nil
}

// Caller returns the workspace's connected client, or nil while unpaired.
func (s *Set) Caller(ws string) Caller {
	s.mu.Lock()
	m := s.managers[ws]
	s.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.Caller()
}

// Status lists every workspace that is paired or pairing.
func (s *Set) Status() []WorkspaceStatus {
	s.mu.Lock()
	out := make([]WorkspaceStatus, 0, len(s.managers))
	for ws, m := range s.managers {
		out = append(out, WorkspaceStatus{Workspace: ws, ManagerStatus: m.Status()})
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].Workspace < out[b].Workspace })
	return out
}

// Pair starts pairing ws with gateway, replacing that workspace's identity.
func (s *Set) Pair(ws, gateway, name string) (*Pairing, error) {
	if err := s.checkWorkspace(ws); err != nil {
		return nil, err
	}
	if name == "" {
		name = "boid"
		if ws != DefaultWorkspace {
			name = "boid-" + ws
		}
	}
	m, err := s.manager(ws)
	if err != nil {
		return nil, err
	}
	return m.Pair(gateway, name)
}

// Forget disconnects ws and deletes its key.
func (s *Set) Forget(ws string) error {
	if !workspaceName.MatchString(ws) {
		return fmt.Errorf("%q is not a workspace name", ws)
	}
	s.mu.Lock()
	m := s.managers[ws]
	delete(s.managers, ws)
	s.mu.Unlock()
	if m != nil {
		return m.Forget()
	}
	if err := os.Remove(s.identityPath(ws)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Set) checkWorkspace(ws string) error {
	if !workspaceName.MatchString(ws) {
		return fmt.Errorf("%q is not a workspace name", ws)
	}
	if s.CheckWorkspace != nil {
		return s.CheckWorkspace(ws)
	}
	return nil
}

// ServeHTTP serves the management API:
// GET /api/tunnel, GET|DELETE /api/tunnel/<workspace>, POST /api/tunnel/<workspace>/pair.
func (s *Set) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tail := strings.Trim(strings.TrimPrefix(r.URL.Path, MountPath), "/")
	ws, action, _ := strings.Cut(tail, "/")
	switch {
	case ws == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Status())
	case ws != "" && action == "" && r.Method == http.MethodGet:
		st := WorkspaceStatus{Workspace: ws}
		for _, e := range s.Status() {
			if e.Workspace == ws {
				st = e
			}
		}
		writeJSON(w, http.StatusOK, st)
	case ws != "" && action == "" && r.Method == http.MethodDelete:
		if err := s.Forget(ws); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case ws != "" && action == "pair" && r.Method == http.MethodPost:
		var in struct {
			Gateway string `json:"gateway"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"gateway": "https://..."}`})
			return
		}
		p, err := s.Pair(ws, in.Gateway, in.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, p)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

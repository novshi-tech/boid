package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newTestClient(transport http.RoundTripper) *Client {
	return &Client{httpClient: &http.Client{Transport: transport}}
}

func TestApplyAction(t *testing.T) {
	wantTaskID := "task-123"
	wantType := "start"

	app := api.ActionApplication{
		Task:   &orchestrator.Task{ID: wantTaskID, Status: orchestrator.TaskStatusExecuting},
		Action: &orchestrator.Action{Type: wantType},
	}
	respJSON, _ := json.Marshal(app)

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		wantPath := "/api/tasks/" + wantTaskID + "/actions"
		if req.URL.Path != wantPath {
			t.Errorf("path: want %q, got %q", wantPath, req.URL.Path)
		}
		if req.Method != http.MethodPost {
			t.Errorf("method: want POST, got %s", req.Method)
		}
		var body api.ApplyActionRequest
		json.NewDecoder(req.Body).Decode(&body)
		if body.Type != wantType {
			t.Errorf("action type: want %q, got %q", wantType, body.Type)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(respJSON)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	result, err := c.ApplyAction(wantTaskID, api.ApplyActionRequest{Type: wantType})
	if err != nil {
		t.Fatalf("ApplyAction error: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Task.ID != wantTaskID {
		t.Errorf("task ID: want %q, got %q", wantTaskID, result.Task.ID)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("task status: want executing, got %q", result.Task.Status)
	}
}

func TestApplyAction_ServerError(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		errBody := `{"error":"task not found"}`
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(errBody)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	_, err := c.ApplyAction("no-such", api.ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("error should mention 'task not found', got %q", err.Error())
	}
}

func TestDeleteTask(t *testing.T) {
	wantTaskID := "task-abc"

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		wantPath := "/api/tasks/" + wantTaskID
		if req.URL.Path != wantPath {
			t.Errorf("path: want %q, got %q", wantPath, req.URL.Path)
		}
		if req.Method != http.MethodDelete {
			t.Errorf("method: want DELETE, got %s", req.Method)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	err := c.DeleteTask(wantTaskID)
	if err != nil {
		t.Fatalf("DeleteTask error: %v", err)
	}
}

func TestDeleteTask_ServerError(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		errBody := `{"error":"task is active"}`
		return &http.Response{
			StatusCode: http.StatusConflict,
			Body:       io.NopCloser(strings.NewReader(errBody)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	err := c.DeleteTask("active-task")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "task is active") {
		t.Errorf("error should mention 'task is active', got %q", err.Error())
	}
}

func TestListWorkspaces(t *testing.T) {
	want := []*orchestrator.WorkspaceSummary{
		{ID: "ws-1", ProjectCount: 2},
		{ID: "ws-2", ProjectCount: 1},
	}
	respJSON, _ := json.Marshal(want)

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/workspaces" {
			t.Errorf("path: want %q, got %q", "/api/workspaces", req.URL.Path)
		}
		if req.Method != http.MethodGet {
			t.Errorf("method: want GET, got %s", req.Method)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(respJSON)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	workspaces, err := c.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces error: %v", err)
	}
	if len(workspaces) != len(want) {
		t.Fatalf("len: want %d, got %d", len(want), len(workspaces))
	}
	for i, ws := range workspaces {
		if ws.ID != want[i].ID {
			t.Errorf("[%d] ID: want %q, got %q", i, want[i].ID, ws.ID)
		}
		if ws.ProjectCount != want[i].ProjectCount {
			t.Errorf("[%d] ProjectCount: want %d, got %d", i, want[i].ProjectCount, ws.ProjectCount)
		}
	}
}

// TUI から実 daemon に届く gate ID は kit-name/gate-name 形式で '/' を含む。
// chi の `{gate_id}` は単一セグメントしかマッチしないので URL エスケープを
// 怠ると 404 になる。client は CLI と同じく PathEscape する責務を負う。
func TestReplayGate_EscapesGateIDPath(t *testing.T) {
	wantTaskID := "task-7c9adc09"
	wantGateID := "github-auto-merge/auto-merge"
	wantPath := "/api/tasks/" + wantTaskID + "/gates/github-auto-merge%2Fauto-merge/replay"

	respJSON, _ := json.Marshal(api.ReplayGateResult{})

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// req.URL.Path は chi が見るのと同じ「未エスケープ」表現を返してしまう
		// ため、生バイト列で検証するには RawPath を使う。
		got := req.URL.RawPath
		if got == "" {
			got = req.URL.Path
		}
		if got != wantPath {
			t.Errorf("path: want %q, got %q", wantPath, got)
		}
		if req.Method != http.MethodPost {
			t.Errorf("method: want POST, got %s", req.Method)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(respJSON)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	if _, err := c.ReplayGate(wantTaskID, wantGateID, ""); err != nil {
		t.Fatalf("ReplayGate error: %v", err)
	}
}

func TestListWorkspaces_ServerError(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		errBody := `{"error":"internal error"}`
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(errBody)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestClient(transport)
	_, err := c.ListWorkspaces()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("error should mention 'internal error', got %q", err.Error())
	}
}

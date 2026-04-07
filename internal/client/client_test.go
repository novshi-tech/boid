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

package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotify_NoCommand(t *testing.T) {
	s := &Service{}
	if err := s.Notify(context.Background(), Event{TaskID: "t1"}); err != nil {
		t.Fatalf("Notify with no command: %v", err)
	}
	var nilSvc *Service
	if err := nilSvc.Notify(context.Background(), Event{TaskID: "t1"}); err != nil {
		t.Fatalf("Notify on nil receiver: %v", err)
	}
}

func TestNotify_ExecCommand(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "script.sh")
	scriptContent := "#!/bin/bash\nprintf 'task=%s title=%s project=%s pname=%s msg=%s url=%s' " +
		"\"$BOID_TASK_ID\" \"$BOID_TASK_TITLE\" \"$BOID_PROJECT_ID\" \"$BOID_PROJECT_NAME\" " +
		"\"$BOID_MESSAGE\" \"$BOID_TASK_URL\" > \"$1\"\n"
	if err := os.WriteFile(script, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	s := &Service{
		Command:   []string{script, out},
		PublicURL: "https://example.com/",
	}
	ev := Event{
		TaskID:      "t1",
		TaskTitle:   "supervisor PR flow",
		ProjectID:   "p1",
		ProjectName: "boid",
		Message:     "hello world",
	}
	if err := s.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	got := string(data)
	want := "task=t1 title=supervisor PR flow project=p1 pname=boid msg=hello world url=https://example.com/tasks/t1"
	if got != want {
		t.Errorf("script output:\n got=%q\nwant=%q", got, want)
	}
}

func TestNotify_NonZeroExit(t *testing.T) {
	s := &Service{Command: []string{"false"}}
	err := s.Notify(context.Background(), Event{TaskID: "t1", Message: "msg"})
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	if !strings.Contains(err.Error(), "notify command") {
		t.Errorf("error message should mention notify command, got: %v", err)
	}
}

func TestNotify_NoPublicURL(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "script.sh")
	scriptContent := "#!/bin/bash\nprintf '%s' \"$BOID_TASK_URL\" > \"$1\"\n"
	if err := os.WriteFile(script, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	s := &Service{Command: []string{script, out}}
	if err := s.Notify(context.Background(), Event{TaskID: "t1", Message: "msg"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	data, _ := os.ReadFile(out)
	if got := string(data); got != "" {
		t.Errorf("BOID_TASK_URL should be empty when PublicURL unset, got=%q", got)
	}
}

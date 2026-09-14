package templates

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWriteCardListFixture(t *testing.T) {
	dir := os.Getenv("BOID_UI_FIXTURE_DIR")
	if dir == "" {
		t.Skip("BOID_UI_FIXTURE_DIR is not set")
	}
	var rows []ListRow
	for _, item := range []struct {
		id    string
		state orchestrator.CardExecutionState
	}{
		{"awaiting", orchestrator.CardExecutionState{Occupied: true, NeedsInput: true}},
		{"discuss", orchestrator.CardExecutionState{Occupied: true, CommandLabel: "Discuss"}},
		{"go", orchestrator.CardExecutionState{Occupied: true}},
		{"idle", orchestrator.CardExecutionState{}},
	} {
		rows = append(rows, ListRow{
			Task:     &orchestrator.Task{ID: item.id, Title: "認証機能の改善", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, UpdatedAt: time.Now()},
			Activity: item.state, Summary: strings.Repeat("長いサマリーの表示を確認します。", 30),
		})
	}
	var page bytes.Buffer
	if err := TaskList(rows, orchestrator.TaskFilter{}, 1, false, nil, nil, "/").Render(context.Background(), &page); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile("../static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "card-list.html"), page.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "style.css"), css, 0o644); err != nil {
		t.Fatal(err)
	}
}

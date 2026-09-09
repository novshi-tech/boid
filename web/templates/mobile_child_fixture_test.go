package templates

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

// TestWriteMobileChildTimelineFixture writes a browser-ready page when
// BOID_UI_FIXTURE_DIR is set. It is opt-in so ordinary unit tests remain
// hermetic; browser checks can serve that directory as their document root.
func TestWriteMobileChildTimelineFixture(t *testing.T) {
	dir := os.Getenv("BOID_UI_FIXTURE_DIR")
	if dir == "" {
		t.Skip("BOID_UI_FIXTURE_DIR is not set")
	}
	items := []timeline.CardItem{
		{
			Kind: timeline.CardItemChild, ID: "child:mobile", CorrelationID: "mobile",
			Child: &timeline.CardChildDetail{
				ChildID: "mobile", Title: "モバイルで子タスクの長い日本語タイトルが一文字幅に潰れず自然に複数行へ折り返されることを確認する",
				Status: "dispatched", LiveStatus: "awaiting", TaskRef: "task-mobile", TaskExists: true,
				Spec: &orchestrator.TaskTriageChildSpec{Behavior: "implementation", Project: "project-with-an-extremely-long-unbroken-name-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
			},
		},
		{
			Kind: timeline.CardItemChild, ID: "child:unbroken", CorrelationID: "unbroken", HasTime: true, Time: time.Now(),
			Child: &timeline.CardChildDetail{
				ChildID: "unbroken", Title: "UNBROKEN-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ-abcdefghijklmnopqrstuvwxyz-0123456789",
				Status: "specced", Spec: &orchestrator.TaskTriageChildSpec{Behavior: "research", Project: "another-extremely-long-unbroken-project-name-0123456789"},
			},
		},
		{
			Kind: timeline.CardItemChildFinished, ID: "finished:mobile", CorrelationID: "mobile", HasTime: true, Time: time.Now().Add(-time.Minute),
			Child: &timeline.CardChildDetail{ChildID: "mobile", Title: "完了した子タスクにも同じ長いタイトル導線が残る", ClosingActionType: "child_closed"},
		},
	}
	body := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		view := &CardTimelineView{
			Pinned:             items[:1],
			History:            items[1:],
			AwaitingQuestionID: "question-mobile",
		}
		task := &orchestrator.Task{ID: "card-mobile", Type: orchestrator.TaskTypeCard, Title: "Mobile child timeline fixture", Status: orchestrator.TaskStatusWorking, Description: "Detailed description preserved during live refresh."}
		identities := []apiwire.TaskIdentity{{Identity: "external:0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", URL: "https://example.com/resource", DisplayName: "Long external resource title 日本語の外部リソース"}}
		receipts := []*orchestrator.OperationResult{{ID: "receipt-mobile", OperationLabel: "Discuss", OperationType: "card_command:discuss", Result: orchestrator.OperationResultRejected, ReasonCode: orchestrator.OperationReasonSlotOccupied, TargetRequestID: "request-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", CreatedAt: time.Now()}}
		return TaskDetailCardBody(task, nil, "", "project", "Summary stays next to its detailed description.", view, []CardCommandOption{{Key: "discuss", Label: "Discuss"}}, nil, identities, receipts).Render(ctx, w)
	})
	ctx := templ.WithChildren(context.Background(), body)
	var page bytes.Buffer
	if err := Layout("Mobile child timeline fixture", "/").Render(ctx, &page); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile(filepath.Join("..", "static", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), page.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "style.css"), css, 0o644); err != nil {
		t.Fatal(err)
	}
}

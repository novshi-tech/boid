package orchestrator_test

// docs/plans/ingestion-identity.md PR-2 (B-2), J-10/A-5: description と
// action payload のサイズ上限。値の根拠 (実測) は content_size.go の
// MaxContentBytes 自身のコメントを参照。

import (
	"strconv"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestValidateContentSize_WithinLimitPasses(t *testing.T) {
	content := strings.Repeat("a", orchestrator.MaxContentBytes)
	if err := orchestrator.ValidateContentSize("description", []byte(content)); err != nil {
		t.Fatalf("ValidateContentSize() at exactly the limit = %v, want nil", err)
	}
}

func TestValidateContentSize_EmptyPasses(t *testing.T) {
	if err := orchestrator.ValidateContentSize("description", nil); err != nil {
		t.Fatalf("ValidateContentSize(nil) = %v, want nil", err)
	}
}

func TestValidateContentSize_OverLimitErrors(t *testing.T) {
	content := strings.Repeat("a", orchestrator.MaxContentBytes+1)
	err := orchestrator.ValidateContentSize("description", []byte(content))
	if err == nil {
		t.Fatal("ValidateContentSize() over the limit = nil, want an error")
	}
	// J-10: 超過時のエラーは「何バイトだったか / 上限は何バイトか」が分かる形にする
	// — workspace が要約に落とすか分割するか選べるように。stderr の verbatim な
	// 文言は公開契約にしない (PR-1 レビュー注意5) が、実際の byte 数と上限値の
	// 両方が含まれていることだけは pin する。
	msg := err.Error()
	if !strings.Contains(msg, "description") {
		t.Errorf("error %q should name the field", msg)
	}
	wantOverBy := len(content)
	if !strings.Contains(msg, strconv.Itoa(wantOverBy)) {
		t.Errorf("error %q should mention the actual byte count %d", msg, wantOverBy)
	}
	if !strings.Contains(msg, strconv.Itoa(orchestrator.MaxContentBytes)) {
		t.Errorf("error %q should mention the limit %d", msg, orchestrator.MaxContentBytes)
	}
}

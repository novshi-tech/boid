package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestCardJudgeReadsCardAndAnswerAttachments(t *testing.T) {
	root := t.TempDir()
	seedAttachment(t, root, "card", "source.png", []byte("card image"))
	seedAttachment(t, root, "card", "shared.txt", []byte("old"))
	seedAttachment(t, root, "judge", "shared.txt", []byte("answer"))
	e := &boidBuiltinExecutor{attachmentsRoot: root}
	ctx := sandbox.TokenContext{TaskID: "judge", CardID: "card", CardRequestID: "request"}
	listed := e.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsList, TaskID: "judge"})
	var names []string
	if err := json.Unmarshal([]byte(listed.Stdout), &names); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"shared.txt", "source.png"}) {
		t.Fatalf("names = %v", names)
	}
	for name, want := range map[string]string{"source.png": "card image", "shared.txt": "answer"} {
		resp := e.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsGet, TaskID: "judge", AttachmentName: name})
		data, err := base64.StdEncoding.DecodeString(resp.Stdout)
		if resp.ExitCode != 0 || err != nil || string(data) != want {
			t.Fatalf("%s: %+v, %v", name, resp, err)
		}
	}
	// No card request context means no additional attachment scope.
	ctx.CardRequestID = ""
	resp := e.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsGet, TaskID: "judge", AttachmentName: "source.png"})
	if resp.ExitCode == 0 {
		t.Fatal("read Card attachment without a Card continuation token")
	}
}

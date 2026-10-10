package gitgateway

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"testing"
)

func TestObserveReceivePack(t *testing.T) {
	var refs []string
	command := "0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/boid/12345678\x00report-status\n"
	second := "1111111111111111111111111111111111111111 0000000000000000000000000000000000000000 refs/heads/other\n"
	body := fmt.Sprintf("%04x%s%04x%s0000PACKremaining", len(command)+4, command, len(second)+4, second)
	got, err := observeReceivePack(io.NopCloser(bytes.NewBufferString(body)), func(ref string) { refs = append(refs, ref) })
	if err != nil {
		t.Fatal(err)
	}
	replay, err := io.ReadAll(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(replay) != body {
		t.Fatal("push body changed")
	}
	if !reflect.DeepEqual(refs, []string{"refs/heads/boid/12345678", "refs/heads/other"}) {
		t.Fatalf("refs=%v", refs)
	}
}

func TestObserveReceivePackMalformed(t *testing.T) {
	for _, body := range []string{"xxxx", "0003", "000cshort", "ffff"} {
		if _, err := observeReceivePack(io.NopCloser(bytes.NewBufferString(body)), func(string) {}); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

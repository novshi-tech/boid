package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestBuildStartupStatus_Migration verifies that a
// *orchestrator.ProjectMigrationError (even wrapped multiple times) is
// recovered and serialised into Kind=migration with every issue preserved
// in order.
func TestBuildStartupStatus_Migration(t *testing.T) {
	mig := &orchestrator.ProjectMigrationError{
		Projects: []orchestrator.ProjectMigrationIssue{
			{ProjectID: "id1", Dir: "/a", Messages: []string{"m1"}},
			{ProjectID: "id2", Dir: "/b", Messages: []string{"m2a", "m2b"}},
		},
	}
	wrapped := fmt.Errorf("create server: %w", mig)

	got := buildStartupStatus(wrapped)
	if got.Kind != StartupKindMigration {
		t.Fatalf("Kind = %q, want %q", got.Kind, StartupKindMigration)
	}
	if len(got.Projects) != 2 {
		t.Fatalf("Projects len = %d, want 2", len(got.Projects))
	}
	if got.Projects[0].ID != "id1" || got.Projects[0].Dir != "/a" {
		t.Fatalf("project[0] = %+v", got.Projects[0])
	}
	if got.Projects[1].Messages[1] != "m2b" {
		t.Fatalf("project[1].Messages[1] = %q, want %q", got.Projects[1].Messages[1], "m2b")
	}
}

// TestBuildStartupStatus_Other captures the fallback path for non-migration
// startup errors. The message must echo err.Error() verbatim so the parent
// can pass it through unchanged.
func TestBuildStartupStatus_Other(t *testing.T) {
	plain := errors.New("disk full")
	got := buildStartupStatus(plain)
	if got.Kind != StartupKindOther {
		t.Fatalf("Kind = %q, want %q", got.Kind, StartupKindOther)
	}
	if got.Message != "disk full" {
		t.Fatalf("Message = %q, want %q", got.Message, "disk full")
	}
}

// TestReadStartupStatus_EOFIsOK pins the ErrStartupOK sentinel — this is
// the contract the parent uses to distinguish "child closed fd 3 quietly
// = success" from any other state.
func TestReadStartupStatus_EOFIsOK(t *testing.T) {
	got, err := ReadStartupStatus(strings.NewReader(""))
	if !errors.Is(err, ErrStartupOK) {
		t.Fatalf("err = %v, want ErrStartupOK", err)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

// TestReadStartupStatus_RoundTrip writes a status, reads it back, and
// asserts the structured shape survives. Mirrors what the parent sees
// when the child writes via WriteStartupStatusOnFD3.
func TestReadStartupStatus_RoundTrip(t *testing.T) {
	want := StartupStatus{
		Kind: StartupKindMigration,
		Projects: []StartupMigrationProject{
			{ID: "id1", Dir: "/x/y", Messages: []string{"m1"}},
		},
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(want); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ReadStartupStatus(&buf)
	if err != nil {
		t.Fatalf("ReadStartupStatus: %v", err)
	}
	if got.Kind != want.Kind || got.Projects[0].ID != "id1" || got.Projects[0].Messages[0] != "m1" {
		t.Fatalf("got = %+v, want = %+v", got, want)
	}
}

// TestReadStartupStatus_GarbageReturnsError makes sure malformed JSON
// (not EOF) is surfaced as a decode error, so callers can distinguish it
// from the success sentinel.
func TestReadStartupStatus_GarbageReturnsError(t *testing.T) {
	got, err := ReadStartupStatus(strings.NewReader("not json"))
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	if errors.Is(err, ErrStartupOK) {
		t.Fatalf("expected non-OK error, got ErrStartupOK")
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

// TestReadStartupStatus_PipeEOF mirrors the parent's real-world setup: an
// os.Pipe whose write-end is closed without writing should surface as
// ErrStartupOK on the read-end.
func TestReadStartupStatus_PipeEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	w.Close() // immediate close → read-end sees EOF

	got, err := ReadStartupStatus(r)
	if !errors.Is(err, ErrStartupOK) {
		t.Fatalf("err = %v, want ErrStartupOK", err)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

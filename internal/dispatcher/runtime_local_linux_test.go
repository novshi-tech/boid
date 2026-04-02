//go:build linux

package dispatcher_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

func TestLocalRuntimeStartWaitAndReplayTranscript(t *testing.T) {
	rootDir := t.TempDir()
	runtime := &dispatcher.LocalRuntime{RootDir: rootDir}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "printf 'hello runtime'",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	result, err := runtime.Wait(context.Background(), handle.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}

	transcriptPath := filepath.Join(rootDir, handle.ID, "transcript.log")
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(data), "hello runtime") {
		t.Fatalf("transcript = %q, want hello runtime", string(data))
	}

	var replay bytes.Buffer
	if err := runtime.Attach(context.Background(), handle.ID, dispatcher.RuntimeAttachRequest{
		Output: &replay,
	}); err != nil {
		t.Fatalf("Attach(replay): %v", err)
	}
	if !strings.Contains(replay.String(), "hello runtime") {
		t.Fatalf("replay output = %q, want hello runtime", replay.String())
	}
}

func TestLocalRuntimeAttachStreamsLiveOutput(t *testing.T) {
	runtime := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "printf 'start'; sleep 0.1; printf ' end'",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var out bytes.Buffer
	attachErrCh := make(chan error, 1)
	go func() {
		attachErrCh <- runtime.Attach(ctx, handle.ID, dispatcher.RuntimeAttachRequest{
			Output: &out,
		})
	}()

	if _, err := runtime.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	select {
	case err := <-attachErrCh:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for attach to finish")
	}

	got := out.String()
	if !strings.Contains(got, "start") || !strings.Contains(got, "end") {
		t.Fatalf("attach output = %q, want streamed transcript", got)
	}
}

func TestLocalRuntimeStopTerminatesProcess(t *testing.T) {
	runtime := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "sleep 30",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Stop(ctx, handle.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	result, err := runtime.Wait(context.Background(), handle.ID)
	if err != nil {
		t.Fatalf("Wait after stop: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("exit code = %d, want non-zero after stop", result.ExitCode)
	}
}

func TestLocalRuntimeResizeOnRunningSession(t *testing.T) {
	runtime := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "sleep 0.2",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := runtime.Resize(context.Background(), handle.ID, dispatcher.TerminalSize{Cols: 120, Rows: 40}); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if _, err := runtime.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

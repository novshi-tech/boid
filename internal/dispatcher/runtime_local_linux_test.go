//go:build linux

package dispatcher_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if result.TranscriptPath != transcriptPath {
		t.Errorf("RuntimeExit.TranscriptPath = %q, want %q", result.TranscriptPath, transcriptPath)
	}
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

func TestLocalRuntimeResizeAfterExit(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "true",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := rt.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Resize after the session has exited must not race and must not error.
	for i := 0; i < 100; i++ {
		if err := rt.Resize(context.Background(), handle.ID, dispatcher.TerminalSize{Cols: 80, Rows: 24}); err != nil {
			t.Fatalf("Resize after exit: %v", err)
		}
	}
}

func TestLocalRuntimeNonInteractiveTranscriptAndReplay(t *testing.T) {
	rootDir := t.TempDir()
	runtime := &dispatcher.LocalRuntime{RootDir: rootDir}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "printf 'hello pipe'",
		Interactive: false,
		TTY:         false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle.Interactive {
		t.Fatalf("handle.Interactive = true, want false")
	}

	result, err := runtime.Wait(context.Background(), handle.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}

	transcriptPath := filepath.Join(rootDir, handle.ID, "transcript.log")
	if result.TranscriptPath != transcriptPath {
		t.Errorf("TranscriptPath = %q, want %q", result.TranscriptPath, transcriptPath)
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(data), "hello pipe") {
		t.Fatalf("transcript = %q, want hello pipe", string(data))
	}

	var replay bytes.Buffer
	if err := runtime.Attach(context.Background(), handle.ID, dispatcher.RuntimeAttachRequest{
		Output: &replay,
	}); err != nil {
		t.Fatalf("Attach(replay): %v", err)
	}
	if !strings.Contains(replay.String(), "hello pipe") {
		t.Fatalf("replay = %q, want hello pipe", replay.String())
	}
}

func TestLocalRuntimeNonInteractiveLiveAttach(t *testing.T) {
	runtime := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "printf 'live'; sleep 0.1; printf ' output'",
		Interactive: false,
		TTY:         false,
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
		t.Fatal("timed out waiting for attach")
	}

	got := out.String()
	if !strings.Contains(got, "live") || !strings.Contains(got, "output") {
		t.Fatalf("attach output = %q, want streamed transcript", got)
	}
}

func TestLocalRuntimeNonInteractiveStopTerminates(t *testing.T) {
	runtime := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "sleep 30",
		Interactive: false,
		TTY:         false,
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

func TestLocalRuntimeNonInteractiveResizeIsNoop(t *testing.T) {
	runtime := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := runtime.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "sleep 0.1",
		Interactive: false,
		TTY:         false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := runtime.Resize(context.Background(), handle.ID, dispatcher.TerminalSize{Cols: 120, Rows: 40}); err != nil {
		t.Fatalf("Resize on non-interactive session: %v", err)
	}

	if _, err := runtime.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Resize after exit must also be no-op.
	if err := runtime.Resize(context.Background(), handle.ID, dispatcher.TerminalSize{Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("Resize after exit: %v", err)
	}
}

// TestLocalRuntimeInteractiveSnapshotIsRenderedGrid verifies that the
// snapshot returned to an attaching client is the resolved screen grid, not
// the raw transcript. A TUI that overwrites in place (here via carriage
// return) must show only the final cells — replaying the raw bytes at a
// different width is what corrupts the mobile web terminal. We use an in-place
// overwrite (not ESC[2J) so nothing scrolls into scrollback, isolating the
// "rendered vs raw" property from the scrollback test below.
func TestLocalRuntimeInteractiveSnapshotIsRenderedGrid(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	// Print OLDLINE, carriage-return to column 0, then overwrite with NEWLINE
	// (same length). On a real terminal only NEWLINE is visible afterwards.
	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     `printf 'OLDLINE\rNEWLINE'`,
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := rt.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	var replay bytes.Buffer
	if err := rt.Attach(context.Background(), handle.ID, dispatcher.RuntimeAttachRequest{
		Output: &replay,
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got := replay.String()
	if !strings.Contains(got, "NEWLINE") {
		t.Fatalf("rendered snapshot missing NEWLINE: %q", got)
	}
	if strings.Contains(got, "OLDLINE") {
		t.Fatalf("rendered snapshot still contains cleared OLDLINE (raw replay, not vt grid): %q", got)
	}
	// The grid is joined with CRLF so a raw-mode xterm starts each row at
	// column 0. A bare LF without a preceding CR would stagger the output.
	if strings.Contains(got, "\n") && !strings.Contains(got, "\r\n") {
		t.Fatalf("rendered snapshot uses bare LF line breaks: %q", got)
	}

	// The raw transcript on disk is untouched — the cleared bytes are still
	// there for `boid job log` / the static /log endpoint.
	raw, err := os.ReadFile(filepath.Join(rt.RootDir, handle.ID, "transcript.log"))
	if err == nil && !strings.Contains(string(raw), "OLDLINE") {
		t.Fatalf("raw transcript.log lost OLDLINE: %q", string(raw))
	}
}

// TestLocalRuntimeNonInteractiveSnapshotIsRaw verifies the vt rendering is NOT
// applied to non-interactive (pipe) sessions. Their transcript is plain log
// output streamed line-by-line (e.g. the SSE /log follow), and collapsing it
// into a screen-sized grid would discard scrolled-off lines.
func TestLocalRuntimeNonInteractiveSnapshotIsRaw(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     `printf 'OLDLINE\rNEWLINE'`,
		Interactive: false,
		TTY:         false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := rt.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	var replay bytes.Buffer
	if err := rt.Attach(context.Background(), handle.ID, dispatcher.RuntimeAttachRequest{
		Output: &replay,
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got := replay.String()
	// Non-interactive output is replayed verbatim, so the pre-clear bytes
	// survive (the escape sequence is just data here, not interpreted).
	if !strings.Contains(got, "OLDLINE") || !strings.Contains(got, "NEWLINE") {
		t.Fatalf("non-interactive snapshot should be raw, got %q", got)
	}
}

// TestLocalRuntimeSnapshotIncludesScrollback verifies that lines which scroll
// off the top of the screen are preserved in the snapshot, so the web client
// can scroll back through history. Rendering only the visible grid would drop
// everything above the current viewport.
func TestLocalRuntimeSnapshotIncludesScrollback(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	// Print 60 numbered lines. The PTY defaults to 24 rows, so the early lines
	// scroll off into the emulator's scrollback while the last ~24 stay on the
	// visible screen.
	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     `for i in $(seq 1 60); do printf 'HISTLINE%d\r\n' "$i"; done`,
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := rt.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	var replay bytes.Buffer
	if err := rt.Attach(context.Background(), handle.ID, dispatcher.RuntimeAttachRequest{
		Output: &replay,
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got := replay.String()
	// HISTLINE60 is on the visible screen.
	if !strings.Contains(got, "HISTLINE60") {
		t.Fatalf("snapshot missing visible HISTLINE60: %q", got)
	}
	// HISTLINE1 scrolled off the top — it must survive via scrollback. The
	// trailing CRLF disambiguates it from HISTLINE10..HISTLINE19.
	if !strings.Contains(got, "HISTLINE1\r\n") {
		t.Fatalf("snapshot dropped scrolled-off HISTLINE1 (no scrollback): %q", got)
	}
}

func TestLocalRuntimeWriteInputParallelNoRace(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	// Start a process that reads stdin and echoes it (cat), keeping the PTY alive.
	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "sleep 2",
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const goroutines = 10
	const writes = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writes; j++ {
				_ = rt.WriteInputRuntime(handle.ID, []byte("x"))
			}
		}()
	}
	wg.Wait()

	// Stop the process and wait for it to exit cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt.Stop(ctx, handle.ID)
	rt.Wait(context.Background(), handle.ID)
}

// TestLocalRuntimeStdinForward_DeliversPipedInput verifies that a
// non-interactive session started with StdinForward: true actually reads
// bytes fed through Attach's RuntimeAttachRequest.Input — the capability
// `boid exec` needs for piped invocations like `echo hi | boid exec cat`.
// Without StdinForward (the hook default) this same input would be silently
// discarded (see TestLocalRuntimeNonInteractiveWithoutStdinForward_DiscardsInput).
func TestLocalRuntimeStdinForward_DeliversPipedInput(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:      "cat",
		Interactive:  false,
		TTY:          false,
		StdinForward: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	attachErrCh := make(chan error, 1)
	go func() {
		attachErrCh <- rt.Attach(ctx, handle.ID, dispatcher.RuntimeAttachRequest{
			Input:  strings.NewReader("piped stdin\n"),
			Output: &out,
		})
	}()

	if _, err := rt.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	select {
	case err := <-attachErrCh:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for attach to finish")
	}

	if !strings.Contains(out.String(), "piped stdin") {
		t.Fatalf("attach output = %q, want cat to echo the forwarded stdin", out.String())
	}
}

// TestLocalRuntimeNonInteractiveWithoutStdinForward_DiscardsInput pins the
// hook-job-compatible default: StdinForward: false (the zero value) must
// keep behaving exactly like before this change — `cat`'s stdin resolves to
// the null device (immediate EOF), so it exits having echoed nothing, even
// though Attach was given real bytes to forward.
func TestLocalRuntimeNonInteractiveWithoutStdinForward_DiscardsInput(t *testing.T) {
	rt := &dispatcher.LocalRuntime{RootDir: t.TempDir()}

	handle, err := rt.Start(context.Background(), dispatcher.RuntimeStartSpec{
		Command:     "cat; printf DONE",
		Interactive: false,
		TTY:         false,
		// StdinForward left false (zero value): matches every hook job.
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	attachErrCh := make(chan error, 1)
	go func() {
		attachErrCh <- rt.Attach(ctx, handle.ID, dispatcher.RuntimeAttachRequest{
			Input:  strings.NewReader("should be discarded"),
			Output: &out,
		})
	}()

	if _, err := rt.Wait(context.Background(), handle.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	select {
	case err := <-attachErrCh:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for attach to finish")
	}

	got := out.String()
	if strings.Contains(got, "should be discarded") {
		t.Fatalf("attach output = %q, forwarded input leaked into a job without StdinForward", got)
	}
	if !strings.Contains(got, "DONE") {
		t.Fatalf("attach output = %q, want DONE (cat must see immediate EOF on the null device)", got)
	}
}

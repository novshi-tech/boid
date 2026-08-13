package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var attachCmd = &cobra.Command{
	Use:         "attach <job-id>",
	Short:       "Attach to a job runtime",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runAttach,
}

func init() {
	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	return attachToJob(cmd.Context(), args[0])
}

// attachToJob is the shared attach core called by `boid attach` and by
// the session-starting subcommands (`boid agent claude`, ...) once they
// have a job id back from POST /api/sessions. It mirrors the original
// runAttach behaviour: a non-running job replays its saved output through
// a pager, a running job opens a live PTY attach with WINCH forwarding.
//
// ctx carries the profile-resolved client (root's PersistentPreRunE) down
// from whichever cobra command originally called in — this and attachLive
// take ctx rather than *cobra.Command because their own callers
// (runAgentSession, runExec) already had to detach from a *cobra.Command at
// their own call boundary (a bare job id is all that survives), so
// threading the narrower context.Context through is both sufficient and
// keeps this file's helpers usable outside a cobra RunE if ever needed.
func attachToJob(ctx context.Context, jobID string) error {
	c := client.FromContext(ctx)

	var job apiwire.Job
	if err := c.Do("GET", "/api/jobs/"+jobID, nil, &job); err != nil {
		return err
	}

	// Non-running jobs: show saved output via pager instead of live attach.
	if job.Status != apiwire.JobStatusRunning {
		return showLogPager(job.Output, os.Stdout, os.Stdin)
	}

	if job.RuntimeID == "" {
		return errors.New("job is not attachable")
	}

	return attachLive(ctx, jobID)
}

// attachLive opens a live attach to jobID (PTY or plain-pipe transport,
// whichever the job was dispatched with) and blocks until the remote side
// closes the stream (job exited — LocalRuntime.Attach replays the transcript
// snapshot even if the job already finished by the time this runs, so there
// is no race to worry about here) or the local user detaches (Ctrl-]).
//
// Callers must already know the job is attachable (RuntimeID set). attachToJob
// checks job.Status / job.RuntimeID first, since a `boid attach <job-id>`
// invocation might target an already-finished or bogus id. `boid exec`
// (cmd/exec.go) skips that check: it always attaches to a job it just
// created via POST .../exec, whose RuntimeID is guaranteed set by the time
// the daemon responds (Runner.Dispatch's launchSandbox persists RuntimeID
// before returning).
func attachLive(ctx context.Context, jobID string) error {
	c := client.FromContext(ctx)

	stdin := io.Reader(os.Stdin)
	// Registered before the raw-mode restore below so it runs after it
	// (defers are LIFO): the terminal is back in cooked mode by the time the
	// epilogue's escape sequences go out.
	defer writeTerminalEpilogueTo(os.Stdout)

	restore, err := makeRawInput(os.Stdin)
	if err != nil {
		return err
	}
	if restore != nil {
		defer restore()
		stdin = &detachReader{reader: os.Stdin}
	}

	sendResize := func() {
		rows, cols, err := terminalSize(os.Stdout)
		if err == nil && rows > 0 && cols > 0 {
			_ = c.ResizeJob(jobID, rows, cols)
		}
	}
	sendResize()

	// Only watch for resizes when stdin is a real terminal (restore != nil);
	// a piped invocation has no window to resize.
	if restore != nil {
		stopWatchingResize := watchTerminalResize(sendResize)
		defer stopWatchingResize()
	}

	// Reconnect notices go to stderr, not stdout: stdout is the job's PTY
	// stream, and splicing boid's own chatter into it would corrupt a TUI
	// harness's screen state.
	return c.AttachJob(jobID, stdin, os.Stdout, client.AttachOptions{Notify: os.Stderr})
}

// writeTerminalEpilogue undoes the terminal modes a job's harness turned on
// through the attach stream but may never get to turn off.
//
// A TUI harness (Claude Code, and anything else bubbletea-shaped) enables
// mouse reporting and bracketed paste on startup by writing DECSET
// sequences to its PTY; those bytes travel down the attach stream and take
// effect on the *local* terminal. The matching DECRST sequences are written
// on the harness's way out — which never happens when the wire dies first.
// The reported symptom is a Windows machine suspending mid-session: on
// resume the attach is gone, and the terminal is still forwarding drags to
// an application that is no longer there, so the user cannot even select
// the error text explaining the drop.
//
// term.Restore (makeRawInput) does not cover this: it restores the console's
// line-discipline state, not modes the remote end set by escape sequence.
//
// Re-sending a reset the harness already sent is a no-op, so this runs
// unconditionally on every attach exit rather than trying to detect an
// unclean one.
func writeTerminalEpilogueTo(f *os.File) {
	if f == nil || !term.IsTerminal(int(f.Fd())) {
		return
	}
	writeTerminalEpilogue(f)
}

// writeTerminalEpilogue writes the reset sequences themselves.
//
// Deliberately absent: DECRST 1049 (leave alternate screen). Switching back
// to the primary buffer would take the drop notice AttachJob just printed
// ("connection lost and could not be restored: ...", client.go) off screen
// along with the rest of the session — the very text the user is trying to
// read and copy. Mouse reporting is what breaks selection; the alternate
// screen does not.
func writeTerminalEpilogue(w io.Writer) {
	_, _ = io.WriteString(w, strings.Join([]string{
		"\x1b[?1000l", // X10/normal mouse tracking off
		"\x1b[?1002l", // button-event tracking off
		"\x1b[?1003l", // any-event tracking off
		"\x1b[?1006l", // SGR extended mouse coordinates off
		"\x1b[?1015l", // urxvt extended mouse coordinates off
		"\x1b[?2004l", // bracketed paste off
		"\x1b[?25h",   // cursor visible
	}, ""))
}

func makeRawInput(f *os.File) (func(), error) {
	if f == nil || !term.IsTerminal(int(f.Fd())) {
		return nil, nil
	}

	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	return func() {
		_ = term.Restore(int(f.Fd()), state)
	}, nil
}

func terminalSize(f *os.File) (rows, cols int, err error) {
	if f == nil || !term.IsTerminal(int(f.Fd())) {
		return 0, 0, nil
	}
	cols, rows, err = term.GetSize(int(f.Fd()))
	return rows, cols, err
}

// showLogPager displays output using a pager ($PAGER → less -R → more).
// Falls back to printing to stdout followed by a "press any key" prompt.
func showLogPager(output string, stdout io.Writer, stdin io.Reader) error {
	return showLogPagerWithCmds(output, stdout, stdin, pagerCommands())
}

// pagerCommands returns the ordered list of pager command+args to try.
func pagerCommands() [][]string {
	var cmds [][]string
	if p := os.Getenv("PAGER"); p != "" {
		cmds = append(cmds, strings.Fields(p))
	}
	cmds = append(cmds, []string{"less", "-R"}, []string{"more"})
	return cmds
}

// showLogPagerWithCmds tries each pagerCmds entry in order, falling back to
// stdout+keypress when none can be found via exec.LookPath.
func showLogPagerWithCmds(output string, stdout io.Writer, stdin io.Reader, pagerCmds [][]string) error {
	for _, args := range pagerCmds {
		if len(args) == 0 {
			continue
		}
		path, err := exec.LookPath(args[0])
		if err != nil {
			continue
		}
		c := exec.Command(path, args[1:]...)
		c.Stdin = strings.NewReader(output)
		c.Stdout = stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	// Fallback: dump to stdout and wait for a keypress.
	fmt.Fprintln(stdout, output)
	fmt.Fprint(stdout, "\n[press any key to close]")
	buf := make([]byte, 1)
	_, _ = stdin.Read(buf)
	fmt.Fprintln(stdout)
	return nil
}

type detachReader struct {
	reader   io.Reader
	detached bool
}

func (r *detachReader) Read(p []byte) (int, error) {
	if r.detached {
		return 0, client.ErrAttachDetached
	}

	buf := make([]byte, len(p))
	n, err := r.reader.Read(buf)
	if n == 0 {
		return 0, err
	}

	for i, b := range buf[:n] {
		if b != 0x1d {
			continue
		}
		r.detached = true
		if i == 0 {
			return 0, client.ErrAttachDetached
		}
		copy(p, buf[:i])
		return i, nil
	}

	copy(p, buf[:n])
	return n, err
}

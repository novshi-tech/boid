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
// the session-starting subcommands once they have a job id back from
// POST /api/sessions: a non-running job replays its saved output through a
// pager, a running job opens a live PTY attach with WINCH forwarding.
//
// Takes ctx (carrying the profile-resolved client) rather than
// *cobra.Command since callers like runAgentSession/runExec only have a
// bare job id left by their own call boundary.
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
// closes the stream (job exited) or the local user detaches (Ctrl-]).
//
// Callers must already know the job is attachable (RuntimeID set) — either
// by checking job.Status/RuntimeID first (attachToJob), or because they
// just created the job themselves and its RuntimeID is guaranteed set by
// the time the daemon responds (`boid exec`).
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
// A TUI harness enables mouse reporting and bracketed paste via DECSET
// sequences that travel down the attach stream and take effect on the
// *local* terminal; the matching DECRST never arrives if the wire dies
// first, leaving the terminal forwarding mouse events to nothing.
//
// term.Restore (makeRawInput) does not cover this: it restores line
// discipline, not modes the remote end set by escape sequence. Re-sending
// an already-sent reset is a no-op, so this runs unconditionally on every
// attach exit.
func writeTerminalEpilogueTo(f *os.File) {
	if f == nil || !term.IsTerminal(int(f.Fd())) {
		return
	}
	writeTerminalEpilogue(f)
}

// writeTerminalEpilogue writes the reset sequences themselves.
//
// Deliberately absent: DECRST 1049 (leave alternate screen). Switching back
// to the primary buffer would take AttachJob's own drop notice off screen
// along with the rest of the session — the very text the user needs to
// read and copy. Mouse reporting is what breaks selection, not the
// alternate screen.
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

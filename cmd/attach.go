package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var attachCmd = &cobra.Command{
	Use:   "attach <job-id>",
	Short: "Attach to a job runtime",
	Args:  cobra.ExactArgs(1),
	RunE:  runAttach,
}

func init() {
	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	return attachToJob(args[0])
}

// attachToJob is the shared attach core called by `boid attach` and by
// the session-starting subcommands (`boid agent claude`, ...) once they
// have a job id back from POST /api/sessions. It mirrors the original
// runAttach behaviour: a non-running job replays its saved output through
// a pager, a running job opens a live PTY attach with WINCH forwarding.
func attachToJob(jobID string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var job api.Job
	if err := c.Do("GET", "/api/jobs/"+jobID, nil, &job); err != nil {
		return err
	}

	// Non-running jobs: show saved output via pager instead of live attach.
	if job.Status != api.JobStatusRunning {
		return showLogPager(job.Output, os.Stdout, os.Stdin)
	}

	if job.RuntimeID == "" {
		return errors.New("job is not attachable")
	}

	return attachLive(jobID)
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
func attachLive(jobID string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	stdin := io.Reader(os.Stdin)
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

	var sigCh chan os.Signal
	if restore != nil {
		sigCh = make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGWINCH)
		defer signal.Stop(sigCh)
		go func() {
			for range sigCh {
				sendResize()
			}
		}()
	}

	return c.AttachJob(jobID, stdin, os.Stdout)
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

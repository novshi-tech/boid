package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// exec.go is the CLI front-end for `boid exec`: a thin wrapper over POST
// /api/projects/{id}/exec, which dispatches through the same Runner.Dispatch()
// a session does.
//
// Known limitation: for a non-interactive (no PTY) exec, stdout and stderr
// are merged into a single stream by the daemon before attachLive writes it
// to os.Stdout — `boid exec -- cmd 2>/dev/null` cannot drop stderr this way,
// and piping to `grep` sees stderr bytes mixed in. Separating them needs
// on-wire stream framing through the runtime interface, the attach
// protocol, and the transcript format — not a one-file fix. The interactive
// (PTY) branch is unaffected: a real terminal always presents one merged
// stream anyway.
var execCmd = &cobra.Command{
	Use:           "exec -p <ref> -- <argv...>",
	Short:         "Run an arbitrary command inside a project sandbox",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.MinimumNArgs(1),
	Annotations:   map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:          runExec,
}

var (
	execProjectRef string
	execName       string
	execReadonly   bool
)

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execProjectRef, "project", "p", "", "project ref (id or name, partial match supported)")
	execCmd.Flags().StringVar(&execName, "name", "", "session display name (defaults to argv[0])")
	execCmd.Flags().BoolVar(&execReadonly, "readonly", false, "mount the project workspace read-only")
	execCmd.Flags().SetInterspersed(false)
	_ = execCmd.RegisterFlagCompletionFunc("project", completeProjectRefs)
}

func runExec(cobraCmd *cobra.Command, args []string) error {
	if execProjectRef == "" {
		return fmt.Errorf("-p/--project is required")
	}

	c := client.FromContext(cobraCmd.Context())

	project, err := resolveProjectRef(c, os.Stdin, os.Stderr, execProjectRef)
	if err != nil {
		return fmt.Errorf("resolve project ref %q: %w", execProjectRef, err)
	}

	// Interactive (PTY) mode requires BOTH stdin and stdout to be a real
	// terminal: allocating a PTY when only stdin is one (e.g. `boid exec --
	// cmd | grep pattern` from an interactive shell) would inject PTY
	// line-discipline framing (echo, extra CR) into the bytes grep receives.
	interactive := isRealTerminal(os.Stdin) && isRealTerminal(os.Stdout)

	req := apiwire.StartExecRequest{
		Argv:        args,
		Readonly:    execReadonly,
		Interactive: interactive,
		DisplayName: execName,
	}
	var result apiwire.StartExecResult
	if err := c.Do("POST", "/api/projects/"+project.ID+"/exec", req, &result); err != nil {
		return fmt.Errorf("start exec: %w", err)
	}

	// attachLive replays-or-streams correctly regardless of timing (see its
	// doc comment in attach.go); no need for attachToJob's GET-then-pager
	// preamble since we already know the job has a runtime.
	if err := attachLive(cobraCmd.Context(), result.JobID); err != nil {
		return fmt.Errorf("attach exec job: %w", err)
	}

	exitCode, err := fetchExecExitCode(cobraCmd.Context(), result.JobID)
	if err != nil {
		return fmt.Errorf("read exec result: %w", err)
	}
	os.Exit(exitCode)
	return nil
}

// isRealTerminal reports whether f is a character device (a real terminal).
func isRealTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// execExitCodeUnknown is the sentinel fetchExecExitCode/pollExecExitCode
// reports when polling gives up without ever observing a terminal job
// status. Deliberately non-zero: the job's zero-value ExitCode looks
// identical to a genuine successful exit, which would make `boid exec`
// os.Exit(0) — a false success — for an outcome that is simply unknown.
const execExitCodeUnknown = 1

// fetchExecExitCode reads back the completed job's exit code. Usually this
// needs exactly one GET: the broker call that persists job.ExitCode happens
// inside the sandboxed process before it can exit, and the attach stream
// (already waited on by the caller) only closes once that process has
// exited — so the DB write has already happened by the time attachLive
// returns. The exception is the "job runtime exited without boid job done"
// fallback path, where completion is instead recorded by a goroutine that
// can race with the attach stream closing — hence polling briefly here
// rather than trusting a single read.
func fetchExecExitCode(ctx context.Context, jobID string) (int, error) {
	c := client.FromContext(ctx)
	return pollExecExitCode(jobID, func() (apiwire.Job, error) {
		var job apiwire.Job
		err := c.Do("GET", "/api/jobs/"+jobID, nil, &job)
		return job, err
	}, time.Sleep)
}

// pollExecExitCode holds fetchExecExitCode's polling loop with the GET call
// and the sleep both injected, so the give-up path (see execExitCodeUnknown)
// is unit-testable without a running daemon or real wall-clock waits.
func pollExecExitCode(jobID string, fetch func() (apiwire.Job, error), sleep func(time.Duration)) (int, error) {
	const maxAttempts = 20
	const pollInterval = 100 * time.Millisecond

	for attempt := 0; ; attempt++ {
		job, err := fetch()
		if err != nil {
			return 0, err
		}
		if job.Status == apiwire.JobStatusCompleted || job.Status == apiwire.JobStatusFailed {
			return job.ExitCode, nil
		}
		if attempt >= maxAttempts-1 {
			// Don't report job.ExitCode (still its zero value) — that would
			// read as a false "succeeded". Fail loud instead.
			fmt.Fprintf(os.Stderr, "boid exec: gave up waiting for job %s to report a definitive exit code; reporting failure\n", jobID)
			return execExitCodeUnknown, nil
		}
		sleep(pollInterval)
	}
}

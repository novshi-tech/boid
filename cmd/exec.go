package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// exec.go is the CLI front-end for `boid exec`. Prior to the git gateway
// cutover's dogfood, this command bypassed the daemon's Job lifecycle
// entirely: the CLI process built its own SandboxRuntimeInfo and
// syscall.Exec'd straight into the sandbox launcher, so it never went
// through Runner.Dispatch() and never picked up dispatch-time wiring added
// there (PR6 added registerGatewayToken / GatewayURL / GatewayCloneURL to
// Dispatch() only — exec's separate path never saw it, which is exactly the
// dogfood bug: `boid exec -p boid bash` failed with "spec.Clone is enabled
// but URL/TargetDir/Branch/BaseBranch must all be set"). Rather than patch
// gateway wiring into this second path, exec is now a thin front-end over
// POST /api/projects/{id}/exec, which dispatches through the exact same
// Runner.Dispatch() a session does — see internal/server/wire.go's
// sessionDispatcherAdapter.StartExec.
var execCmd = &cobra.Command{
	Use:           "exec -p <ref> -- <argv...>",
	Short:         "Run an arbitrary command inside a project sandbox",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.MinimumNArgs(1),
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

	c := client.NewUnixClient(client.DefaultSocketPath())

	// GET /api/projects/{ref} resolves partial/name refs server-side (the
	// same resolveRef every other project route uses) and returns the
	// project with workspace meta (host_commands / env / additional_bindings
	// / capabilities) already hydrated, so there is nothing left for the CLI
	// to fetch or merge itself — Dispatch() reads all of that straight from
	// the daemon's own project store.
	project, err := resolveProjectRef(c, os.Stdin, os.Stderr, execProjectRef)
	if err != nil {
		return fmt.Errorf("resolve project ref %q: %w", execProjectRef, err)
	}

	// Interactive (PTY) mode requires BOTH stdin and stdout to be a real
	// terminal, not just stdin. Checking stdin alone (the pre-cutover
	// heuristic) was harmless there only because syscall.Exec inherited the
	// CLI's raw fds unconditionally regardless of this flag's value — it had
	// no actual effect on I/O. Now it selects a real PTY vs. plain-pipe
	// transport at the daemon (see runtime_local_linux.go), so getting it
	// wrong has a real, visible consequence: `boid exec -- cmd | grep
	// pattern` run from an interactive shell still has a terminal on stdin,
	// but allocating a PTY there would inject PTY line-discipline framing
	// (echo, extra CR) into the bytes grep receives. Requiring stdout to also
	// be a terminal avoids that.
	interactive := isRealTerminal(os.Stdin) && isRealTerminal(os.Stdout)

	req := api.StartExecRequest{
		Argv:        args,
		Readonly:    execReadonly,
		Interactive: interactive,
		DisplayName: execName,
	}
	var result api.StartExecResult
	if err := c.Do("POST", "/api/projects/"+project.ID+"/exec", req, &result); err != nil {
		return fmt.Errorf("start exec: %w", err)
	}

	// attachLive always replays-or-streams correctly regardless of timing
	// (LocalRuntime.Attach snapshots the transcript-so-far even if the
	// process already exited) — see attachLive's doc comment in attach.go for
	// why the RuntimeID-set guarantee lets us skip attachToJob's
	// GET-then-pager preamble entirely and go straight to the live attach.
	if err := attachLive(result.JobID); err != nil {
		return fmt.Errorf("attach exec job: %w", err)
	}

	exitCode, err := fetchExecExitCode(result.JobID)
	if err != nil {
		return fmt.Errorf("read exec result: %w", err)
	}
	os.Exit(exitCode)
	return nil
}

// isRealTerminal reports whether f is a character device (a real terminal),
// mirroring the pre-cutover isatty check in this file — same detection
// method, just applied to both stdin and stdout (see runExec's comment on
// why both now matter).
func isRealTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// fetchExecExitCode reads back the completed job's exit code. In the
// overwhelmingly common case this needs exactly one GET: postJobDone's HTTP
// round-trip to the broker (which durably persists job.ExitCode via
// TaskWorkflowService.CompleteJob) happens *inside* the sandboxed process,
// strictly before that process can exit — and the attach stream (which
// fetchExecExitCode's caller has already waited on) only closes once the
// runtime's top-level process has exited. So by the time attachLive returns,
// the DB write has already happened.
//
// The one edge case where that ordering guarantee doesn't hold is the
// existing "job runtime exited without boid job done" fallback (see
// Runner.watchRuntime): if postJobDone's broker call itself failed (broker
// unreachable, token invalidated, ...), completion is instead recorded by a
// separate goroutine that races with — and can run after — the attach stream
// closing. That fallback path is not new to exec (every session job shares
// it), but exec is the first caller that actually needs the exit code
// synchronously, so this polls briefly rather than trusting a single read.
func fetchExecExitCode(jobID string) (int, error) {
	c := client.NewUnixClient(client.DefaultSocketPath())

	const maxAttempts = 20
	const pollInterval = 100 * time.Millisecond

	var job api.Job
	for attempt := 0; ; attempt++ {
		if err := c.Do("GET", "/api/jobs/"+jobID, nil, &job); err != nil {
			return 0, err
		}
		if job.Status == api.JobStatusCompleted || job.Status == api.JobStatusFailed {
			return job.ExitCode, nil
		}
		if attempt >= maxAttempts-1 {
			// Give up waiting for the fallback path and report whatever exit
			// code is on record (0 if none) rather than blocking forever.
			return job.ExitCode, nil
		}
		time.Sleep(pollInterval)
	}
}

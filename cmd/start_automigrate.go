package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/novshi-tech/boid/internal/daemon"
)

// autoMigratePrompter asks the user whether to proceed. Returns
// (proceed, err); a true value runs migrations, false aborts. Injected to
// keep handleMigrationFailure testable without a real TTY.
type autoMigratePrompter func(out io.Writer, in io.Reader) (bool, error)

// migrateRunnerFunc is the in-process MigrateProject entry point. Injected
// so unit tests can stub it without touching the filesystem / DB.
type migrateRunnerFunc func(opts MigrateProjectOptions) error

// handleMigrationFailure renders the migration summary, optionally prompts
// the user, and runs migrations one by one. On full success it returns
// (retrySpawn=true, nil) so the parent select loop can respawn the daemon
// child. Any failure / decline / non-TTY-without-flag returns (false, err);
// the parent surfaces err and exits.
//
// Behaviour matrix:
//
//	autoYes | isTTY | action
//	--------+-------+----------------------------------------------
//	  false |  true | summary → prompt → migrate (if y) or decline
//	  false | false | summary → suggest --auto-migrate; abort
//	   true |  true | summary → migrate without prompt
//	   true | false | summary → migrate without prompt (CI / scripts)
//
// "any failure aborts the loop" is the contract the user agreed to: if
// project N fails (e.g. secret collision under --on-collision=refuse) we
// stop immediately and do not attempt projects N+1.. .
func handleMigrationFailure(
	out io.Writer,
	in io.Reader,
	status *daemon.StartupStatus,
	logPath string,
	autoYes bool,
	isTTY bool,
	prompt autoMigratePrompter,
	runMigrate migrateRunnerFunc,
) (retrySpawn bool, err error) {
	renderMigrationSummary(out, status)

	// Non-TTY without the flag: we cannot safely confirm. Print the
	// manual remediation lines and bail with a hint at --auto-migrate.
	if !isTTY && !autoYes {
		printManualRemediation(out, status, logPath)
		return false, errors.New("boid start: project migration needed (re-run with --auto-migrate to apply automatically, or migrate manually)")
	}

	// Confirm unless --auto-migrate is set.
	if !autoYes {
		ok, perr := prompt(out, in)
		if perr != nil {
			return false, fmt.Errorf("auto-migrate prompt: %w", perr)
		}
		if !ok {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Auto-migrate declined. To migrate manually:")
			for _, p := range status.Projects {
				fmt.Fprintf(out, "  boid project migrate %s --apply\n", p.Dir)
			}
			return false, errors.New("boid start: auto-migrate declined by user")
		}
	} else {
		fmt.Fprintln(out, "\n--auto-migrate: applying migrations without confirmation")
	}

	return runAutoMigrate(out, status, runMigrate)
}

// runAutoMigrate executes MigrateProject for every issue in order. It stops
// at the first failure and surfaces the failing project's dir + index in
// the returned error so the user knows where to resume manually.
func runAutoMigrate(out io.Writer, status *daemon.StartupStatus, runMigrate migrateRunnerFunc) (bool, error) {
	total := len(status.Projects)
	for i, p := range status.Projects {
		fmt.Fprintf(out, "\n[%d/%d] migrating %s\n", i+1, total, p.Dir)
		opts := MigrateProjectOptions{
			Dir:         p.Dir,
			Apply:       true,
			OnCollision: "refuse",
			Out:         out,
		}
		if err := runMigrate(opts); err != nil {
			fmt.Fprintf(out, "[%d/%d] FAILED: %s\n", i+1, total, p.Dir)
			return false, fmt.Errorf("auto-migrate aborted at %q (%d/%d): %w",
				p.Dir, i+1, total, err)
		}
		fmt.Fprintf(out, "[%d/%d] OK: %s\n", i+1, total, p.Dir)
	}
	fmt.Fprintf(out, "\nAll %d migration(s) applied. Restarting daemon...\n", total)
	return true, nil
}

// renderMigrationSummary writes a human-readable per-project listing of
// the migration issues plus a short list of the consequences of running
// auto-migrate. It is the one place where the daemon's structured status
// is rendered to a user-facing TTY-or-pipe sink.
func renderMigrationSummary(out io.Writer, status *daemon.StartupStatus) {
	fmt.Fprintf(out, "boid start: %d project(s) need migration to the new project.yaml schema.\n\n",
		len(status.Projects))
	for _, p := range status.Projects {
		if p.ID != "" {
			fmt.Fprintf(out, "  - %s\n    %s\n", p.Dir, p.ID)
		} else {
			fmt.Fprintf(out, "  - %s\n", p.Dir)
		}
		for _, m := range p.Messages {
			fmt.Fprintf(out, "      %s\n", m)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Auto-migrate will:")
	fmt.Fprintln(out, "  - run `boid project migrate <dir> --apply` for each project")
	fmt.Fprintln(out, "  - merge each project's env / kits / capabilities into the target workspace.yaml")
	fmt.Fprintln(out, "    (env keys are last-write-wins when several projects share a workspace)")
	fmt.Fprintln(out, "  - rewrite each project.yaml atomically; no backup is taken")
	fmt.Fprintln(out, "  - stop on the first failure (e.g. secret collision under --on-collision=refuse)")
}

func printManualRemediation(out io.Writer, status *daemon.StartupStatus, logPath string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Re-run with `boid start --auto-migrate` to apply automatically, or run each manually:")
	for _, p := range status.Projects {
		fmt.Fprintf(out, "  boid project migrate %s --apply\n", p.Dir)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Full daemon log: "+logPath)
}

// defaultMigratePrompter reads a y/N answer from in. Anything other than
// "y" / "yes" (case-insensitive) is treated as decline, mirroring the
// `[y/N]` convention shown in the prompt.
func defaultMigratePrompter(out io.Writer, in io.Reader) (bool, error) {
	fmt.Fprint(out, "\nProceed? [y/N]: ")
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return false, nil
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}

package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/humanize"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// `boid workspace import-home <slug> [--from <dir>]` — PR8 of
// docs/plans/workspace-home-volume-persistence.md (論点 f).
//
// PR6 moved every workspace's $HOME into a docker named volume, which fixed
// the persistence regression the plan doc exists for and left the CONTENTS of
// the pre-PR6 host directories — the harness authentication and the installed
// toolchain — where they were. This command moves them across, once.
//
// # What this process does and does not do (D1)
//
// It reads the host directory and streams a tar. It does NOT talk to the docker
// engine, does not delete anything, and does not decide anything about markers
// or identities. All of that is the daemon's
// (dispatcher.Runner.ImportWorkspaceHome), because all of the state it has to
// keep consistent — the completion marker inside the daemon's own volume, the
// per-workspace init flock, the volume's identity label — is the daemon's.
// `boid reap` is the counter-example that proves the rule: it drives the engine
// from the CLI precisely because it must work when the daemon is DOWN, which is
// the opposite of this command's requirement.

var (
	// workspaceImportHomeFrom is --from: the legacy host directory to read.
	workspaceImportHomeFrom string
	// workspaceImportHomeYes is --yes/--force: skip the confirmation prompt.
	workspaceImportHomeYes bool
	// workspaceImportHomeDryRun is --dry-run: walk the source and report what
	// would move, without sending anything.
	workspaceImportHomeDryRun bool
)

// workspaceImportHomeConfirmPrompt is the y/N reader, injectable for tests —
// the same seam, for the same reason, as workspaceRemoveConfirmPrompt.
var workspaceImportHomeConfirmPrompt = defaultWorkspaceImportHomeConfirmPrompt

func defaultWorkspaceImportHomeConfirmPrompt(in io.Reader) (bool, error) {
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return false, nil
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}

var workspaceImportHomeCmd = &cobra.Command{
	Use:   "import-home <slug> [--from <dir>]",
	Short: "Migrate a pre-volume workspace HOME directory into the workspace's HOME volume (POST /api/workspaces/{slug}/home/import)",
	Long: `Copy the contents of a legacy host-side workspace home
(~/.local/share/boid/homes/<slug>) into the workspace's docker named volume,
replacing whatever is in it.

This is a one-off migration for installations that predate the move of the
workspace $HOME onto a named volume (docs/plans/workspace-home-volume-
persistence.md). Those directories still hold each workspace's harness
authentication and its installed toolchain; the volume the daemon now mounts
starts out empty.

How it works: this command reads <dir> and streams it to the daemon as a tar.
The daemon replaces the workspace's HOME volume with a brand-new one and
extracts the archive into it. It never bind-mounts the host directory, because
under rootless podman the container's uid is not the host's and a 0600
credentials file would be unreadable across that mapping.

SAFETY

  - <dir> is only ever READ. Nothing deletes it, and re-running is safe.
  - A <dir> that is itself a symlink is FOLLOWED, and where it resolves to is
    shown in the prompt and in --dry-run. Symlinks inside the home are not
    followed; they cross as symlinks.
  - A <dir> with nothing to migrate is REFUSED before anything is sent. An
    empty archive would replace the workspace's home with an empty one and
    report success, because the daemon destroys the volume before it reads the
    body. To start a home over deliberately, use "boid workspace remove".
  - A workspace with a job running in it is REFUSED, with nothing changed:
    the engine will not remove a volume a live container is using.
  - The existing HOME volume is DESTROYED and replaced. There is no undo, and
    the confirmation prompt is shown unless --yes is given.
  - If the transfer fails partway, the workspace is left as if it had never
    been dispatched into: the next dispatch re-runs its init.sh from scratch.
    The legacy directory is still there, so the migration can just be re-run.

AFTER THE MIGRATION the workspace's init.sh runs once on the next dispatch —
deliberately, so anything the old home baked host-era absolute paths into is
rewritten. It is a short run for an idempotent script over an
already-populated home.

  boid workspace import-home team-a                      # from ~/.local/share/boid/homes/team-a
  boid workspace import-home team-a --from /mnt/backup/team-a
  boid workspace import-home team-a --dry-run            # show what would move
  boid workspace import-home team-a --yes                # no confirmation prompt`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceImportHome,
}

func init() {
	// scopeRemote: the work happens through the daemon's HTTP API. It reads a
	// local directory to build the payload, which is what `project init` is
	// classified scopeLocal for — but the difference is which HOST the path has
	// to exist on. `project init` resolves a path the DAEMON then opens, so it
	// only works when the two share a filesystem; this one resolves a path
	// THIS process opens and sends the bytes, which is exactly why the payload
	// is a tar stream. Against a remote daemon it does the right thing.
	workspaceImportHomeCmd.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}

	workspaceImportHomeCmd.Flags().StringVar(&workspaceImportHomeFrom, "from", "",
		"directory to import (default: the legacy ~/.local/share/boid/homes/<slug>)")
	workspaceImportHomeCmd.Flags().BoolVar(&workspaceImportHomeYes, "yes", false,
		"skip the confirmation prompt (the existing HOME volume is destroyed and replaced)")
	workspaceImportHomeCmd.Flags().BoolVar(&workspaceImportHomeYes, "force", false, "alias for --yes")
	workspaceImportHomeCmd.Flags().BoolVar(&workspaceImportHomeDryRun, "dry-run", false,
		"report what would be sent and stop; nothing is transferred and nothing is destroyed")

	workspaceCmd.AddCommand(workspaceImportHomeCmd)
}

// defaultWorkspaceHomeSource resolves --from's default: the legacy
// homes/<slug> directory on THIS host.
//
// dispatcher.WorkspaceHomesDir("") is the authority rather than a literal path
// assembled here, so the CLI and the daemon-side function that documents this
// layout cannot drift. The empty argument is deliberate and correct for a CLI:
// that parameter is the daemon's RuntimesDir, which this process has no way to
// know and which — since PR6 — resolves under $XDG_RUNTIME_DIR anyway. The
// fallback branch it takes ($XDG_DATA_HOME/boid/homes, else
// ~/.local/share/boid/homes) is exactly where a pre-PR6 install put them, and
// is where the 5 measured homes on the dogfood machine are.
func defaultWorkspaceHomeSource(slug string) (string, error) {
	root, err := dispatcher.WorkspaceHomesDir("")
	if err != nil {
		return "", err
	}
	return filepath.Join(root, slug), nil
}

func runWorkspaceImportHome(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	typed := workspaceImportHomeFrom
	if typed == "" {
		resolved, err := defaultWorkspaceHomeSource(slug)
		if err != nil {
			return fmt.Errorf("resolve the default --from directory: %w", err)
		}
		typed = resolved
	}
	typed = filepath.Clean(typed)

	// Refused HERE, before anything is sent. The daemon destroys the workspace's
	// HOME volume before it reads a single byte of the archive (that removal is
	// also its in-use check — see dispatcher.Runner.ImportWorkspaceHome), so a
	// typo'd --from that this process merely passed along would cost the
	// operator their home for nothing.
	//
	// The check goes through dispatcher.ResolveWorkspaceHomeSource rather than a
	// local os.Stat, and that is the whole of codex round 3's Blocker 2 on this
	// side: os.Stat FOLLOWS a symlink and reports a directory, while the walk that
	// builds the archive does not follow one — so `--from` pointing at a symlink
	// used to pass every check here and then produce an archive with no entries.
	// One function decides it for both, so they cannot disagree again.
	resolved, err := dispatcher.ResolveWorkspaceHomeSource(typed)
	if err != nil {
		return fmt.Errorf("%w (pass --from if the legacy home is somewhere else, e.g. a backup directory)", err)
	}
	src := workspaceHomeSource{Typed: typed, Resolved: resolved}

	// The second half of Blocker 2, and the one that does not depend on knowing
	// WHY the source is empty. Whatever the cause — a symlink an older build
	// failed to follow, a typo, a backup that was never populated, a mount that
	// is not mounted — the outcome is identical and uniquely destructive: the
	// daemon removes the HOME volume before it reads the body, so an archive with
	// nothing in it replaces a workspace's credentials and toolchain with an empty
	// home and answers 200. The daemon refuses it too (that is the guard on the
	// same side as the deletion); this one refuses it before an operator is even
	// asked to confirm.
	//
	// Checked above the --dry-run branch on purpose: --dry-run exists to predict
	// what a real run would do, and a dry-run that cheerfully reported "0 files"
	// for a source a real run would reject predicts the wrong thing.
	hasEntries, err := dispatcher.WorkspaceHomeSourceHasEntries(src.Resolved)
	if err != nil {
		return fmt.Errorf("read the workspace home source %s: %w", src, err)
	}
	if !hasEntries {
		return fmt.Errorf(
			"the workspace home source %s holds nothing this migration could carry, so completing it would replace "+
				"workspace %q's HOME volume with an empty one — its harness authentication and its installed toolchain "+
				"would be gone. Nothing was sent. Check --from (a typo, an unmounted backup, or a directory that was never "+
				"populated); to deliberately start this workspace's home over, use `boid workspace remove %[2]s` and "+
				"re-create it instead", src, slug)
	}

	if workspaceImportHomeDryRun {
		return workspaceImportHomeDryRunReport(cmd, slug, src)
	}

	if !workspaceImportHomeYes {
		fmt.Fprintf(out, "workspace %q: import %s into its HOME volume?\n", slug, src)
		fmt.Fprintf(out, "  the workspace's CURRENT home volume will be destroyed and replaced (no undo),\n")
		fmt.Fprintf(out, "  %s is only read, and init.sh will run once on the next dispatch.\n", src)
		fmt.Fprint(out, "continue? [y/N]: ")
		proceed, perr := workspaceImportHomeConfirmPrompt(os.Stdin)
		if perr != nil {
			return fmt.Errorf("confirm prompt: %w", perr)
		}
		if !proceed {
			fmt.Fprintf(out, "aborted: workspace %q was not touched\n", slug)
			return nil
		}
	}

	return streamWorkspaceHomeImport(cmd, slug, src)
}

// workspaceHomeSource is a validated --from: the path the operator typed, and
// the directory it actually resolves to.
//
// The two are kept apart rather than collapsed because both are load-bearing and
// for different readers. The RESOLVED path is what gets walked (a symlinked root
// is followed — see dispatcher.ResolveWorkspaceHomeSource). The TYPED one is what
// the operator recognizes, and silently replacing it in the confirmation prompt
// would hide the one fact that prompt exists to establish: which bytes are about
// to overwrite an irreplaceable home.
type workspaceHomeSource struct {
	Typed    string
	Resolved string
}

// String renders the pair for every operator-facing message: just the path when
// there is nothing to disclose, and `typed (-> resolved)` when following a
// symlink changed where the bytes come from.
func (s workspaceHomeSource) String() string {
	if s.Resolved == "" || s.Resolved == s.Typed {
		return s.Typed
	}
	return fmt.Sprintf("%s (-> %s)", s.Typed, s.Resolved)
}

// workspaceImportHomeDryRunReport walks the source and reports what a real run
// would send, without opening a connection.
//
// It is a full walk rather than an estimate because the numbers are the point:
// an operator comparing "what is about to be sent" against `boid workspace
// show`'s reported volume size needs both to mean the same thing, and
// WorkspaceHomeTarStats.Bytes is deliberately counted the way the engine's own
// `docker system df` counts (a hard-linked payload once).
func workspaceImportHomeDryRunReport(cmd *cobra.Command, slug string, src workspaceHomeSource) error {
	out := cmd.OutOrStdout()
	stats, err := dispatcher.WriteWorkspaceHomeTar(io.Discard, src.Resolved, nil)
	if err != nil {
		return fmt.Errorf("read the workspace home source %s: %w", src, err)
	}
	fmt.Fprintf(out, "dry-run: would import %s into workspace %q's HOME volume\n", src, slug)
	fmt.Fprint(out, formatWorkspaceHomeTarStats(stats))
	fmt.Fprintf(out, "  nothing was sent and nothing was destroyed; re-run without --dry-run to migrate\n")
	return nil
}

// streamWorkspaceHomeImport builds the tar and POSTs it, without ever holding
// the archive in memory.
//
// The io.Pipe is what makes that true: the writer goroutine walks the source
// into one end, and net/http reads the other as the request body. A failure in
// the walk closes the pipe with its own error, so the request aborts rather
// than delivering a truncated archive the daemon would extract into the volume
// it has already replaced.
func streamWorkspaceHomeImport(cmd *cobra.Command, slug string, src workspaceHomeSource) error {
	out := cmd.OutOrStdout()
	c := client.FromContext(cmd.Context())

	// Printed BEFORE the writer goroutine starts, so this function never writes
	// to out concurrently with the progress callback below (out is a
	// bytes.Buffer under test).
	fmt.Fprintf(out, "importing %s into workspace %q's HOME volume...\n", src, slug)

	pr, pw := io.Pipe()
	var (
		stats    dispatcher.WorkspaceHomeTarStats
		writeErr error
	)
	// Closed once stats/writeErr have been assigned. Not implied by the pipe:
	// net/http closes the request body itself when the exchange ends, which
	// unblocks a pending Write with ErrClosedPipe and would let this function
	// read those variables while the goroutine is still assigning them.
	done := make(chan struct{})
	go func() {
		defer close(done)
		stats, writeErr = dispatcher.WriteWorkspaceHomeTar(pw, src.Resolved, func(p dispatcher.WorkspaceHomeTarStats) {
			// Progress on stdout, not a log: a 4.3GB transfer with no output is
			// indistinguishable from a hang (D4), and this is the only place
			// that knows how far along it is.
			fmt.Fprintf(out, "  ... %d files, %s read\n", p.Files, humanize.FormatBytes(p.Bytes))
		})
		_ = pw.CloseWithError(writeErr)
	}()

	status, body, err := c.PostStream(cmd.Context(), "/api/workspaces/"+slug+"/home/import",
		workspaceHomeImportMediaType, pr)
	// Unblock the writer if the transport abandoned the request early (a 409
	// answered before the upload finished, a dropped connection): a closed read
	// end makes every further Write return, so the goroutine always reaches its
	// own completion rather than parking on a pipe nobody reads.
	_ = pr.Close()
	<-done

	if failure := workspaceHomeImportFailure(src.String(), status, body, err, writeErr); failure != nil {
		return failure
	}

	var resp api.WorkspaceHomeImportResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	fmt.Fprint(out, formatWorkspaceHomeTarStats(stats))
	fmt.Fprint(out, formatWorkspaceHomeImportResult(slug, resp))
	return nil
}

// workspaceHomeImportMediaType mirrors the daemon's own required media type
// (internal/api's workspaceHomeImportContentType, unexported there).
const workspaceHomeImportMediaType = "application/x-tar"

// workspaceHomeImportFailure decides WHICH of up to three concurrent failures
// is the one worth reporting, and returns nil when there was none.
//
// The precedence exists because of one specific interaction that gets the most
// important message in this command wrong if it is left to fall out naturally.
// The daemon answers a workspace with a running job at the very START of the
// request — the volume removal is its in-use check — so a 409 arrives while the
// CLI is still uploading. net/http surfaces that response and abandons the body,
// this function's caller then closes the pipe to unblock its writer, and the
// walk comes back with io.ErrClosedPipe. Reporting the walk's error first turns
//
//	refusing to migrate while a job is running in it
//
// into
//
//	read the workspace home source "...": io: read/write on closed pipe
//
// which sends the operator to look at their own filesystem for a problem that
// is not there. So:
//
//  1. A response the SERVER produced wins. It is an answer about the migration
//     itself, and it is the only place a 409/404/500 message exists.
//  2. Otherwise a genuine walk failure wins over the transport error it caused:
//     a permission-denied file makes the request fail with a body-read error
//     that describes nothing.
//  3. io.ErrClosedPipe is never a walk failure. It is this command closing its
//     own read end, and it says only that the exchange ended first.
func workspaceHomeImportFailure(source string, status int, body []byte, reqErr, writeErr error) error {
	if errors.Is(writeErr, io.ErrClosedPipe) {
		writeErr = nil
	}
	if reqErr == nil && status != http.StatusOK {
		return fmt.Errorf("import workspace home: %s", formatWorkspaceAPIError(status, body))
	}
	if writeErr != nil {
		return fmt.Errorf("read the workspace home source %q: %w", source, writeErr)
	}
	if reqErr != nil {
		return fmt.Errorf("import workspace home: %w", reqErr)
	}
	return nil
}

func formatWorkspaceHomeTarStats(s dispatcher.WorkspaceHomeTarStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %d files, %d dirs, %d symlinks (%d hard links), %s\n",
		s.Files, s.Dirs, s.Symlinks, s.HardLinks, humanize.FormatBytes(s.Bytes))
	if len(s.Skipped) > 0 {
		// Named, not counted: an operator has to be able to decide whether
		// anything that did not cross mattered. Sockets and FIFOs are the
		// expected content here and none of them is restorable.
		fmt.Fprintf(&b, "  skipped %d entries that cannot be restored:\n", len(s.Skipped))
		for _, entry := range s.Skipped {
			fmt.Fprintf(&b, "    %s\n", entry)
		}
	}
	return b.String()
}

// formatWorkspaceHomeImportResult renders the daemon's report.
//
// The init.sh line is not chatter. 論点 f's acceptance condition is that the
// migration re-arms the workspace's init exactly once, and an operator who is
// not told that will read the next dispatch's toolchain re-check as a bug.
//
// The marker line only appears when leg 2 did NOT fire: both legs are meant to
// be redundant, so "only one of them is holding this up" is the state worth
// naming.
func formatWorkspaceHomeImportResult(slug string, resp api.WorkspaceHomeImportResponse) string {
	var b strings.Builder
	if resp.PreviousExisted {
		fmt.Fprintf(&b, "workspace %q home imported: volume %s replaced\n", slug, resp.Volume)
	} else {
		fmt.Fprintf(&b, "workspace %q home imported: volume %s created (the workspace had never been dispatched into)\n",
			slug, resp.Volume)
	}
	if !resp.MarkerRemoved {
		fmt.Fprintf(&b, "warning: the init completion marker could not be removed (%s)\n", resp.MarkerRemoveError)
		fmt.Fprintf(&b, "  not fatal: the replacement volume has a new identity, which re-runs init.sh on its own\n")
	}
	// The leftover in-progress record. The daemon writes one before it destroys
	// anything and clears it at the end, so a record it could not clear is
	// leftover state worth naming — but WHAT it costs is the whole message, and
	// since round 4 of the codex review it is usually nothing.
	//
	// The severe branch is the one whose consequence lands later and looks
	// unrelated: a record still in the phase that says this home is wreckage
	// makes the next `boid start` discard a multi-GB import. Saying that for the
	// cheap case instead would be worse than saying nothing — under the compose
	// deploy the file it tells them to delete is inside the daemon's own volume,
	// so an operator who cannot reach it learns to ignore the line that
	// eventually matters.
	if resp.MigrationRecordRemoveError != "" {
		fmt.Fprintf(&b, "warning: the daemon could not clear its in-progress migration record (%s)\n",
			resp.MigrationRecordRemoveError)
		if resp.MigrationRecordDiscardsHome {
			fmt.Fprintf(&b, "  the import itself succeeded, but the NEXT daemon start will treat that record as an "+
				"unfinished migration and discard this home. Delete the file named above before restarting the daemon\n")
		} else {
			fmt.Fprintf(&b, "  the import itself succeeded and this home is safe: the record no longer says the home was "+
				"destroyed, so the next daemon start just deletes it. Nothing to do — but the daemon's state area could "+
				"not be written to, which is worth looking into\n")
		}
	}
	fmt.Fprintf(&b, "init.sh will run once on the next dispatch into %q (this is intended — it rewrites anything the "+
		"old home baked host-era absolute paths into)\n", slug)
	return b.String()
}

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// `boid workspace get-init-script / set-init-script / edit-init-script /
// unset-init-script` (docs/plans/workspace-home-volume-persistence.md
// 論点 d) manage a workspace's init.sh — which installs the harness CLI and
// toolchain into the workspace HOME volume — through the daemon's API,
// since the daemon reads it from its own config directory, which under a
// containerized daemon is unreachable from this host. These follow `boid
// config`'s conventions (GET with an ETag, write gated by If-Match,
// --force to opt out, $EDITOR round trip that keeps the temp file on
// failure), since it is the same problem: a file the daemon owns, edited
// from elsewhere.
//
// unset exists as its own verb (rather than folding into set) because "no
// init.sh" is a real, distinct state that set alone reaches only via an
// unobvious route (uploading an empty file).
//
// There is no `set-init-script --from-stdin`-only variant: -f accepts "-"
// for stdin, which is the same convention with one fewer flag.

var (
	// workspaceInitScriptFile is set-init-script's required -f/--file: the
	// script to upload, or "-" for stdin.
	workspaceInitScriptFile string
	// workspaceInitScriptOutput is get-init-script's optional -o/--output.
	workspaceInitScriptOutput string
	// workspaceInitScriptForce skips the If-Match revision check on
	// set/unset/edit, for a deliberate last-write-wins write — the same
	// --force `boid workspace edit` and `boid config apply` carry.
	workspaceInitScriptForce bool
)

var workspaceGetInitScriptCmd = &cobra.Command{
	Use:   "get-init-script <slug> [-o file]",
	Short: "Print a workspace's init.sh as the daemon holds it (GET /api/workspaces/{slug}/init-script)",
	Long: `Print the init.sh the daemon runs when it first prepares this workspace's
HOME volume.

The script lives with the DAEMON, not on this machine: the daemon reads it
from its own config directory, which for a containerized daemon is inside its
state volume. That is why it is fetched over the API rather than read from
~/.config/boid/workspaces/<slug>/init.sh here.

A workspace with no init.sh exits non-zero rather than printing nothing. An
empty script is not a state boid keeps — uploading one clears the script
instead (see set-init-script) — so empty output could only ever mean "there is
no script", and saying so on stderr with a non-zero exit is how a pipeline
finds out.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceGetInitScript,
}

var workspaceSetInitScriptCmd = &cobra.Command{
	Use:   "set-init-script <slug> -f <file>",
	Short: "Replace a workspace's init.sh from a local file (PUT /api/workspaces/{slug}/init-script, automatic If-Match)",
	Long: `Upload a local file as the workspace's init.sh. Pass -f - to read the script
from stdin.

The script runs once, inside a throwaway container, the next time this
workspace is dispatched into — and again whenever its content changes. It is
NOT executed as a file: boid reads the bytes and feeds them to bash, so the
shebang line has no effect and the file needs no executable bit.

The current revision is fetched first and sent back as If-Match, so a script
that changed on the daemon since this command started is reported instead of
silently overwritten. --force skips that check.

An empty file CLEARS the workspace's init.sh (same as unset-init-script): an
empty script and no script are the same thing to dispatch, so boid does not
keep two representations of it.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceSetInitScript,
}

var workspaceEditInitScriptCmd = &cobra.Command{
	Use:   "edit-init-script <slug>",
	Short: "Edit a workspace's init.sh in $EDITOR (or vi) and apply it on save",
	Long: `Fetch the workspace's init.sh, open it in $EDITOR (falling back to vi), and
apply it if it changed — with the revision captured at the start sent as
If-Match, so an edit that took a while does not clobber a change that landed
meanwhile.

A workspace with no init.sh yet opens an empty buffer; saving it unchanged is
a no-op.

If the edit cannot be applied (it is invalid, or the script changed on the
daemon), the edited file is KEPT and its path reported, so the work is not
lost.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceEditInitScript,
}

var workspaceUnsetInitScriptCmd = &cobra.Command{
	Use:   "unset-init-script <slug>",
	Short: "Remove a workspace's init.sh, returning it to the no-script pass-through state",
	Long: `Delete the workspace's init.sh on the daemon.

The workspace then joins the pass-through class: dispatch still prepares its
HOME volume (the bind-target skeleton is created either way), it just runs no
script. The HOME volume itself and everything already installed in it are NOT
touched — use "boid workspace remove" for that.

The current revision is sent as If-Match, so this cannot delete a script that
changed since it was last read. --force skips that check.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceUnsetInitScript,
}

func init() {
	for _, c := range []*cobra.Command{
		workspaceGetInitScriptCmd, workspaceSetInitScriptCmd,
		workspaceEditInitScriptCmd, workspaceUnsetInitScriptCmd,
	} {
		// scopeRemote, like every other `boid workspace *`: the script lives
		// with the daemon and every one of these is an API call.
		c.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	}

	workspaceGetInitScriptCmd.Flags().StringVarP(&workspaceInitScriptOutput, "output", "o", "",
		"file to write the script to (default: stdout)")
	workspaceSetInitScriptCmd.Flags().StringVarP(&workspaceInitScriptFile, "file", "f", "",
		"script file to upload, or \"-\" for stdin (required)")
	workspaceSetInitScriptCmd.Flags().BoolVar(&workspaceInitScriptForce, "force", false,
		"write without checking the current revision (last-write-wins; skips the concurrency guard)")
	workspaceEditInitScriptCmd.Flags().BoolVar(&workspaceInitScriptForce, "force", false,
		"apply without checking the current revision (last-write-wins)")
	workspaceUnsetInitScriptCmd.Flags().BoolVar(&workspaceInitScriptForce, "force", false,
		"remove without checking the current revision (last-write-wins)")

	workspaceCmd.AddCommand(
		workspaceGetInitScriptCmd,
		workspaceSetInitScriptCmd,
		workspaceEditInitScriptCmd,
		workspaceUnsetInitScriptCmd,
	)
}

// workspaceInitScriptPathFor builds the endpoint path for slug.
func workspaceInitScriptPathFor(slug string) string {
	return "/api/workspaces/" + slug + "/init-script"
}

// fetchedWorkspaceInitScript is what one GET of the init-script endpoint
// yields.
type fetchedWorkspaceInitScript struct {
	// Content is the script, empty when Exists is false.
	Content []byte
	// Exists is false for a workspace in the pass-through class.
	Exists bool
	// Revision is the ETag to send back as If-Match — a content hash, or
	// apiwire.WorkspaceInitScriptAbsentRevision when there is no script. Both are
	// usable: guarding the CREATION of a workspace's first script is exactly
	// what the absent sentinel is for.
	//
	// Kept in the QUOTED form the header carried, not unwrapped — an
	// entity-tag is DQUOTE-delimited per RFC 7232 §2.3, and a strict
	// intermediary in front of a remote daemon can reject a bare token.
	// Nothing on this side compares it; it only travels back out as
	// If-Match, which the daemon's unquoteETag unwraps.
	Revision string
	// Message is the daemon's own 404 text, which names the daemon-side path.
	Message string
}

// fetchWorkspaceInitScript performs one GET and interprets the two success
// shapes (200 with the script, 404 with the absent sentinel) plus the one
// failure that looks like a success and is not.
//
// # Telling "no script" from "no such endpoint"
//
// Both arrive as 404. A daemon predating this endpoint has no route at all,
// so chi answers with its own not-found and no ETag; this daemon always
// sets the header, even on the 404. An empty revision on a 404 is therefore
// the discriminator, and it gets its own message pointing at the upgrade —
// the same explicit "the daemon is too old" handling resolveDaemonKitsDir
// already does for its endpoint, rather than the silent guess that would
// otherwise send an operator hunting for a workspace that is right there.
func fetchWorkspaceInitScript(c *client.Client, slug string) (fetchedWorkspaceInitScript, error) {
	statusCode, body, revision, err := c.GetRawWithAcceptAndRevision(
		workspaceInitScriptPathFor(slug), apiwire.WorkspaceInitScriptContentType)
	if err != nil {
		return fetchedWorkspaceInitScript{}, fmt.Errorf("fetch init.sh: %w", err)
	}
	revision = strings.TrimSpace(revision)

	switch {
	case statusCode == http.StatusOK:
		return fetchedWorkspaceInitScript{Content: body, Exists: true, Revision: revision}, nil
	case statusCode == http.StatusNotFound && revision != "":
		return fetchedWorkspaceInitScript{Exists: false, Revision: revision, Message: initScriptAPIMessage(body)}, nil
	case statusCode == http.StatusNotFound:
		return fetchedWorkspaceInitScript{}, fmt.Errorf(
			"fetch init.sh: the daemon did not answer %s (HTTP 404 with no ETag) — either workspace %q does not exist, "+
				"or this daemon predates `boid workspace set-init-script` (upgrade it, then retry)",
			workspaceInitScriptPathFor(slug), slug)
	default:
		return fetchedWorkspaceInitScript{}, fmt.Errorf("fetch init.sh: %s", formatWorkspaceAPIError(statusCode, body))
	}
}

// initScriptAPIMessage extracts the daemon's `{"error": "..."}` text, or
// returns the raw body when it is not that shape.
func initScriptAPIMessage(body []byte) string {
	var resp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.Error != "" {
		return resp.Error
	}
	return strings.TrimSpace(string(body))
}

// validateWorkspaceInitScriptLocally is the client-side half of the
// double-validation this repo does everywhere (`workspace edit`, `workspace
// import`, `config apply`): the daemon runs the identical check, but failing
// here saves a round trip and works with no daemon reachable.
//
// Only the NUL byte, for the reasons internal/api's
// validateWorkspaceInitScriptContent spells out — it is the only one of
// dispatch's four fail-closed conditions decidable from the content alone.
func validateWorkspaceInitScriptLocally(content []byte) error {
	if i := bytes.IndexByte(content, 0); i >= 0 {
		return fmt.Errorf("init.sh contains a NUL byte at offset %d; a shell script must be text", i)
	}
	return nil
}

func runWorkspaceGetInitScript(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())
	script, err := fetchWorkspaceInitScript(c, slug)
	if err != nil {
		return err
	}
	if !script.Exists {
		if script.Message != "" {
			return fmt.Errorf("%s", script.Message)
		}
		return fmt.Errorf("workspace %q has no init.sh", slug)
	}

	if workspaceInitScriptOutput != "" {
		// 0644, not 0600: this is a copy on the OPERATOR's machine for
		// editing, not the daemon's authoritative file (which the store
		// publishes 0600). Matching `workspace export -o`'s mode keeps the two
		// "pull it down to a file" flags consistent.
		if err := os.WriteFile(workspaceInitScriptOutput, script.Content, 0o644); err != nil {
			return fmt.Errorf("write -o/--output: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %d bytes to %s\n", len(script.Content), workspaceInitScriptOutput)
		return nil
	}
	_, err = cmd.OutOrStdout().Write(script.Content)
	return err
}

func runWorkspaceSetInitScript(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if workspaceInitScriptFile == "" {
		return fmt.Errorf("-f/--file is required (pass \"-\" to read the script from stdin)")
	}

	var content []byte
	var err error
	if workspaceInitScriptFile == "-" {
		content, err = io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("read script from stdin: %w", err)
		}
	} else {
		content, err = os.ReadFile(workspaceInitScriptFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", workspaceInitScriptFile, err)
		}
	}

	// Before any daemon call — see validateWorkspaceInitScriptLocally.
	if err := validateWorkspaceInitScriptLocally(content); err != nil {
		return fmt.Errorf("validate %s: %w", workspaceInitScriptFile, err)
	}

	c := client.FromContext(cmd.Context())
	ifMatch, err := currentWorkspaceInitScriptRevision(c, slug)
	if err != nil {
		return err
	}
	return putWorkspaceInitScript(cmd, c, slug, content, ifMatch, "")
}

func runWorkspaceUnsetInitScript(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())
	ifMatch, err := currentWorkspaceInitScriptRevision(c, slug)
	if err != nil {
		return err
	}
	// An empty body is the clear (see internal/api's
	// workspaceInitScriptContentIsAbsent) — there is no DELETE verb on this
	// route, because "write nothing" and "delete" would then be two ways to
	// say one thing and would have to be kept in agreement forever.
	return putWorkspaceInitScript(cmd, c, slug, nil, ifMatch, "")
}

// currentWorkspaceInitScriptRevision fetches the revision to use as If-Match,
// or "" when --force was passed (no check requested, so no fetch either).
func currentWorkspaceInitScriptRevision(c *client.Client, slug string) (string, error) {
	if workspaceInitScriptForce {
		return "", nil
	}
	script, err := fetchWorkspaceInitScript(c, slug)
	if err != nil {
		return "", err
	}
	return script.Revision, nil
}

// putWorkspaceInitScript performs the write and reports the outcome.
//
// keptPath, when non-empty, is a temp file the caller wants named in any error
// (edit-init-script's kept edit); it is appended to every failure message
// rather than handled by the caller so no failure path can forget it.
func putWorkspaceInitScript(cmd *cobra.Command, c *client.Client, slug string, content []byte, ifMatch, keptPath string) error {
	path := workspaceInitScriptPathFor(slug)
	if workspaceInitScriptForce {
		path += "?force=true"
	}
	statusCode, body, err := c.PutRawWithIfMatch(path, apiwire.WorkspaceInitScriptContentType, content, ifMatch)
	if err != nil {
		return initScriptError(fmt.Errorf("write init.sh: %w", err), keptPath)
	}
	if statusCode == http.StatusPreconditionRequired || statusCode == http.StatusPreconditionFailed {
		return initScriptError(fmt.Errorf(
			"workspace %q's init.sh changed since this command read it; re-run it (or pass --force to overwrite unconditionally): %s",
			slug, formatWorkspaceAPIError(statusCode, body)), keptPath)
	}
	if statusCode != http.StatusOK {
		return initScriptError(fmt.Errorf("write init.sh: %s", formatWorkspaceAPIError(statusCode, body)), keptPath)
	}

	var result apiwire.WorkspaceInitScriptResult
	if err := json.Unmarshal(body, &result); err != nil {
		return initScriptError(fmt.Errorf("decode response: %w", err), keptPath)
	}
	fmt.Fprint(cmd.OutOrStdout(), formatWorkspaceInitScriptResult(slug, result))
	return nil
}

// initScriptError appends the kept-edit note to err when there is one.
func initScriptError(err error, keptPath string) error {
	if keptPath == "" {
		return err
	}
	return fmt.Errorf("%w (edited init.sh kept at %s)", err, keptPath)
}

// formatWorkspaceInitScriptResult renders the outcome line(s) of a write.
//
// The daemon-side path is printed on every outcome, labelled as the daemon's.
// An operator whose daemon is a container has no way to check the file
// themselves and every reason to want to know where it went — and an
// unlabelled path invites the mistake of reading a daemon-side path as a
// host one.
func formatWorkspaceInitScriptResult(slug string, result apiwire.WorkspaceInitScriptResult) string {
	var b strings.Builder
	switch result.Action {
	case apiwire.WorkspaceInitScriptWritten:
		fmt.Fprintf(&b, "workspace %q init.sh written (%d bytes)\n", slug, result.Bytes)
		fmt.Fprintf(&b, "  it runs on the next dispatch into this workspace, in a throwaway container\n")
	case apiwire.WorkspaceInitScriptCleared:
		fmt.Fprintf(&b, "workspace %q init.sh cleared — the workspace now runs no init script\n", slug)
		fmt.Fprintf(&b, "  the HOME volume and everything installed in it are untouched\n")
	case apiwire.WorkspaceInitScriptUnchanged:
		fmt.Fprintf(&b, "workspace %q init.sh unchanged\n", slug)
	default:
		fmt.Fprintf(&b, "workspace %q init.sh: %s\n", slug, result.Action)
	}
	if result.Path != "" {
		fmt.Fprintf(&b, "  daemon-side location: %s (inside the daemon's own filesystem, not this host's)\n", result.Path)
	}
	return b.String()
}

// runWorkspaceEditInitScript implements `boid workspace edit-init-script`: a
// straight adaptation of runConfigEdit (cmd/config.go), including its failure
// discipline — a validation OR conflict failure keeps the temp file and
// reports its path rather than silently discarding the edit.
//
// The one difference from `config edit` is the starting buffer: a workspace
// with no init.sh yet opens an EMPTY file rather than 404ing, because creating
// the first script is the common case here (a fresh workspace has none) and
// there is nothing to fetch. Saving that empty buffer unchanged is a no-op,
// which is also the correct answer for "I opened it to look".
func runWorkspaceEditInitScript(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())
	script, err := fetchWorkspaceInitScript(c, slug)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "boid-workspace-init-*.sh")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(script.Content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("$EDITOR is set to an empty/whitespace-only value")
	}
	editCmd := exec.Command(parts[0], append(append([]string(nil), parts[1:]...), tmpPath)...)
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr
	if err := editCmd.Run(); err != nil {
		return fmt.Errorf("run editor %q: %w (edited init.sh kept at %s)", editor, err, tmpPath)
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("read edited file: %w", err)
	}

	// Byte-exact comparison, NOT the trimmed one `config edit` uses: this
	// content is hashed into the workspace's completion marker, so a trailing
	// newline added or removed is a real change that re-runs init — treating
	// it as "no changes" would silently drop an edit the operator made.
	if bytes.Equal(script.Content, edited) {
		_ = os.Remove(tmpPath)
		fmt.Fprintln(cmd.OutOrStdout(), "no changes")
		return nil
	}

	if err := validateWorkspaceInitScriptLocally(edited); err != nil {
		return fmt.Errorf("validation failed: %w (edited init.sh kept at %s — fix it and rerun `boid workspace set-init-script %s -f %s`)",
			err, tmpPath, slug, tmpPath)
	}

	if err := putWorkspaceInitScript(cmd, c, slug, edited, script.Revision, tmpPath); err != nil {
		return err
	}
	_ = os.Remove(tmpPath)
	return nil
}

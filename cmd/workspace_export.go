package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// runWorkspaceExport exports one workspace (the <slug> positional argument)
// or every workspace (--all) as a boid.dev/v1 Workspace yaml document
// (docs/plans/volume-only-daemon.md §論点g). Unlike every other workspace
// subcommand, this deliberately does not go through renderOutput: the whole
// point is to emit yaml (to stdout, or -o/--output <path>), not a
// json/yaml/plain-text rendering of a structured response object.
//
// Both the single-slug and --all cases hit GET /api/workspaces/export (the
// atomic snapshot endpoint, docs/plans/volume-only-daemon.md PR-1d codex
// round-1 Blocker 3) rather than composing the document client-side from
// separate GET /api/workspaces/{slug} + GET /api/projects?workspace_id=...
// calls: the old per-workspace two-request loop could straddle a concurrent
// `workspace assign` moving a project between two workspaces mid-export,
// losing or duplicating that project across the exported documents. A
// single daemon-side transaction (orchestrator.SnapshotWorkspacesForExport)
// closes that window.
func runWorkspaceExport(cmd *cobra.Command, args []string) error {
	var path string
	if workspaceExportAll {
		if len(args) != 0 {
			return fmt.Errorf("workspace export --all takes no <slug> argument")
		}
		path = "/api/workspaces/export?all=true"
	} else {
		if len(args) != 1 {
			return fmt.Errorf("workspace export requires exactly one <slug> argument, or --all")
		}
		if err := orchestrator.ValidWorkspaceSlug(args[0]); err != nil {
			return err
		}
		path = "/api/workspaces/export?name=" + args[0]
	}

	c := client.FromContext(cmd.Context())
	stderr := cmd.ErrOrStderr()

	statusCode, output, err := c.GetRaw(path)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("export: %s", formatWorkspaceAPIError(statusCode, output))
	}

	docCount, err := checkExportedWorkspaceEnvelopes(stderr, output)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}

	if workspaceExportOutput != "" {
		if err := os.WriteFile(workspaceExportOutput, output, 0o644); err != nil {
			return fmt.Errorf("write -o/--output: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "exported %d workspace(s) to %s\n", docCount, workspaceExportOutput)
		return nil
	}

	_, err = cmd.OutOrStdout().Write(output)
	return err
}

// checkExportedWorkspaceEnvelopes decodes the raw export response with the
// SAME decoder `boid workspace apply -f` uses, requires every document to be a
// COMPLETE one, returns the number of documents found (for the "exported N
// workspace(s)" message), and prints the two advisory warnings the pre-atomic
// client-side export used to (a project with no captured upstream_url yet, an
// env value that looks like a host filesystem path).
//
// The decode is a GATE, not a best-effort courtesy (PR9 codex round 4, Major 1).
// It used to swallow its own error and return 0, so a 200 whose body is not a
// restorable set of Workspace documents — a daemon bug, a proxy's error page,
// an emitter defect like the one workspace_envelope_marshal.go exists for — was
// written to -o/--output and announced as "exported 0 workspace(s)", a line an
// operator reads as success. `boid workspace export` is the endorsed backup
// path (docs/plans/volume-only-daemon.md §論点g); a file it wrote that its own
// `apply` cannot read is the one outcome that must never be silent, and the
// only moment it can be caught is before the file exists.
//
// An EMPTY body is refused by the same reasoning, and by the decoder itself: a
// zero-byte backup restores nothing, and `apply` rejects it.
//
// The WARNINGS stay best-effort in the other direction — they are advisory, and
// nothing below can fail the export once the documents passed the checks above.
// They are also printed only AFTER every document has been accepted: a refused
// export wrote no file, so warnings about what is in it would describe
// something that does not exist.
func checkExportedWorkspaceEnvelopes(stderr io.Writer, data []byte) (int, error) {
	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		return 0, fmt.Errorf(
			"the daemon's response is not a set of Workspace documents this boid can apply, so nothing was written: %w", err)
	}
	for _, doc := range docs {
		if err := checkExportedDocumentIsComplete(doc); err != nil {
			return 0, err
		}
	}
	for _, doc := range docs {
		slug := doc.Envelope.Metadata.Name
		warnEnvHostPaths(stderr, doc.Envelope.Spec.Env)
		for _, p := range doc.Envelope.Spec.Projects {
			if p.URL == "" {
				fmt.Fprintf(stderr, "warning: project %q in workspace %q has no known git remote URL yet (pre-PR-2) — url omitted from export, will be filled in once `boid project add`/`reload` captures it\n", p.Name, slug)
			}
		}
	}
	return len(docs), nil
}

// checkExportedDocumentIsComplete requires one exported document to carry
// spec.init_script (PR9 codex round 4 final, Major 1) — the version-skew half
// of "a file this command wrote can always be restored".
//
// # Why decoding successfully is not enough
//
// The decoder deliberately treats a missing spec.init_script as a legitimate
// value: "leave this workspace's init.sh alone" (see
// orchestrator.WorkspaceEnvelopeSpec.InitScript). So a response that predates
// PR9 — a daemon with no notion of an init.sh at all — decodes cleanly, applies
// cleanly, and restores a workspace whose HOME volume then has no toolchain in
// it and whose next dispatch cannot start a harness. The decode gate above
// cannot see that: it asks whether `apply` can READ the response, and the answer
// is yes.
//
// The skew is reachable rather than theoretical. Every `boid workspace *`
// command is scopeRemote, so a new CLI routinely talks to whatever daemon is at
// the other end of the socket, and this PR's own init-script commands already
// carry explicit handling for a daemon that predates them
// (fetchWorkspaceInitScript's "HTTP 404 with no ETag" branch).
//
// # Why this is an export-side requirement only
//
// `boid workspace apply` must keep ACCEPTING a document with no
// spec.init_script, and TestWorkspaceApply_AcceptsADocumentWithNoInitScriptKey
// pins that it does. The two surfaces read the same absence differently because
// they are answering different questions:
//
//   - apply reads a document a human may have WRITTEN, to state a change. One
//     that adjusts env alone has no business restating an init.sh it is not
//     touching, and "absent = don't touch" is what makes a partial document
//     expressible at all. Requiring the key here would force every hand-written
//     apply file to carry a copy of the whole script;
//   - export writes a document meant to RESTORE the workspace as a whole. There
//     is no partial export — the daemon emits every field on every document —
//     so an absent key cannot mean "don't touch"; it can only mean the daemon
//     did not know the field exists.
func checkExportedDocumentIsComplete(doc *orchestrator.WorkspaceEnvelopeApply) error {
	if doc.FieldsPresent["init_script"] {
		return nil
	}
	return fmt.Errorf(
		"the daemon's export of workspace %q carries no spec.init_script, so it is not a complete backup: "+
			"restoring it would bring the workspace's settings back WITHOUT its init.sh, leaving its HOME volume with no "+
			"toolchain and no harness. This daemon predates `boid workspace set-init-script` — upgrade the daemon, then "+
			"re-run this export. Nothing was written",
		doc.Envelope.Metadata.Name)
}

// hostPathPattern matches an env value that looks like it references a host
// filesystem path (docs/plans/volume-only-daemon.md §論点g「env の host path
// 依存」) — an absolute path under /home, /opt, /mnt, or /root, either at the
// start of the value or after a ':' (PATH-style) or '=' separator. This is a
// heuristic advisory check, not a validator: it deliberately also flags
// container-internal paths that happen to share a prefix, EXCEPT the ones
// containerSafeHostPathPrefixes whitelists (Minor 1, codex round-1) — export
// still proceeds either way.
var hostPathPattern = regexp.MustCompile(`(?:^|[:=])(/(?:home|opt|mnt|root)(?:/[^:=]*)?)`)

// containerSafeHostPathPrefixes are absolute path prefixes that superficially
// match hostPathPattern but are legitimate, valid-inside-the-sandbox paths,
// not a leaked host filesystem reference (Minor 1, codex round-1): the boid
// user's home inside the container is /home/boid, so e.g.
// "GOPATH: /home/boid/go" is exactly the plan doc's own documented example
// of a correct workspace env value, not a mistake to warn about.
var containerSafeHostPathPrefixes = []string{
	"/home/boid/",
	"/opt/boid/",
	"/usr/local/",
	"/tmp/",
}

// warnEnvHostPaths prints one stderr warning line per env entry whose value
// looks like a host filesystem path (and is not one of
// containerSafeHostPathPrefixes), in sorted key order for deterministic
// output.
func warnEnvHostPaths(stderr io.Writer, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := env[k]
		for _, m := range hostPathPattern.FindAllStringSubmatch(v, -1) {
			if containerSafeHostPath(m[1]) {
				continue
			}
			fmt.Fprintf(stderr, "warning: env value %s=%s looks like a host filesystem path — not valid in container. Consider providing via a kit image layer.\n", k, v)
			break
		}
	}
}

// containerSafeHostPath reports whether p (a path hostPathPattern matched)
// is one of containerSafeHostPathPrefixes' allowed prefixes.
func containerSafeHostPath(p string) bool {
	for _, prefix := range containerSafeHostPathPrefixes {
		if p == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

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
// Both the single-slug and --all cases hit GET /api/workspaces/export — a
// single atomic daemon-side snapshot (orchestrator.SnapshotWorkspacesForExport)
// rather than a per-workspace GET+GET loop that could straddle a concurrent
// `workspace assign` and lose or duplicate a project across the export.
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
// workspace(s)" message), and prints two advisory warnings (a project with no
// captured upstream_url yet, an env value that looks like a host filesystem
// path).
//
// The decode is a GATE, not a best-effort courtesy: a response that is not a
// restorable set of Workspace documents must fail loudly here, before a file
// claiming "exported N workspace(s)" gets written to -o/--output — the only
// moment that failure can still be caught. An EMPTY body is refused the same
// way, and by the decoder itself.
//
// The WARNINGS stay best-effort in the other direction, and are printed only
// after every document has been accepted.
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
// spec.init_script, the version-skew half of "a file this command wrote can
// always be restored": the decoder treats a missing spec.init_script as
// "leave this workspace's init.sh alone" for `apply` (which must keep
// accepting a hand-written document that never mentions it), but for
// `export` — which always emits every field — an absent key can only mean
// the daemon predates init.sh support, and restoring such a document would
// silently leave the workspace's HOME volume with no toolchain.
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
// filesystem path: an absolute path under /home, /opt, /mnt, or /root,
// either at the start of the value or after a ':' (PATH-style) or '='
// separator. This is a heuristic advisory check, not a validator — export
// still proceeds either way — and containerSafeHostPathPrefixes exempts
// paths that are valid inside the sandbox despite matching.
var hostPathPattern = regexp.MustCompile(`(?:^|[:=])(/(?:home|opt|mnt|root)(?:/[^:=]*)?)`)

// containerSafeHostPathPrefixes are absolute path prefixes that superficially
// match hostPathPattern but are legitimate, valid-inside-the-sandbox paths,
// not a leaked host filesystem reference: the boid user's home inside the
// container is /home/boid, so e.g. "GOPATH: /home/boid/go" is a correct
// workspace env value, not a mistake to warn about.
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

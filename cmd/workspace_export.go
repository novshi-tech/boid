package cmd

import (
	"bytes"
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

	docCount := warnExportedWorkspaceEnvelopes(stderr, output)

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

// warnExportedWorkspaceEnvelopes decodes the raw export response and prints
// the same two advisory warnings the pre-atomic client-side export used to
// (a project with no captured upstream_url yet, an env value that looks
// like a host filesystem path), then returns the number of documents found
// (for the "exported N workspace(s)" message). Warnings are best-effort: a
// decode failure here (which would mean the daemon emitted something that
// is not a valid Workspace document) is silently skipped rather than
// failing the export — the raw bytes the daemon returned are still written
// out untouched either way.
func warnExportedWorkspaceEnvelopes(stderr io.Writer, data []byte) int {
	if len(bytes.TrimSpace(data)) == 0 {
		return 0
	}
	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		return 0
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
	return len(docs)
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
